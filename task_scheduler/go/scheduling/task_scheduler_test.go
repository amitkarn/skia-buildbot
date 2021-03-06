package scheduling

import (
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	buildbucket_api "github.com/luci/luci-go/common/api/buildbucket/buildbucket/v1"
	swarming_api "github.com/luci/luci-go/common/api/swarming/swarming/v1"
	assert "github.com/stretchr/testify/require"
	depot_tools_testutils "go.skia.org/infra/go/depot_tools/testutils"
	"go.skia.org/infra/go/gerrit"
	"go.skia.org/infra/go/git"
	"go.skia.org/infra/go/git/repograph"
	git_testutils "go.skia.org/infra/go/git/testutils"
	"go.skia.org/infra/go/isolate"
	"go.skia.org/infra/go/mockhttpclient"
	"go.skia.org/infra/go/swarming"
	"go.skia.org/infra/go/testutils"
	"go.skia.org/infra/go/util"
	"go.skia.org/infra/task_scheduler/go/blacklist"
	"go.skia.org/infra/task_scheduler/go/db"
	"go.skia.org/infra/task_scheduler/go/specs"
	specs_testutils "go.skia.org/infra/task_scheduler/go/specs/testutils"
	"go.skia.org/infra/task_scheduler/go/tryjobs"
	"go.skia.org/infra/task_scheduler/go/window"
)

const (
	fakeGerritUrl = "https://fake-skia-review.googlesource.com"
)

var (
	androidTaskDims = map[string]string{
		"pool":        "Skia",
		"os":          "Android",
		"device_type": "grouper",
	}

	linuxTaskDims = map[string]string{
		"os":   "Ubuntu",
		"pool": "Skia",
	}

	androidBotDims = map[string][]string{
		"pool":        {"Skia"},
		"os":          {"Android"},
		"device_type": {"grouper"},
	}

	linuxBotDims = map[string][]string{
		"os":   {"Ubuntu"},
		"pool": {"Skia"},
	}
)

func getCommit(t *testing.T, gb *git_testutils.GitBuilder, commit string) string {
	co := git.Checkout{
		GitDir: git.GitDir(gb.Dir()),
	}
	rv, err := co.RevParse(commit)
	assert.NoError(t, err)
	return rv
}

func getRS1(t *testing.T, gb *git_testutils.GitBuilder) db.RepoState {
	return db.RepoState{
		Repo:     gb.RepoUrl(),
		Revision: getCommit(t, gb, "HEAD^"),
	}
}

func getRS2(t *testing.T, gb *git_testutils.GitBuilder) db.RepoState {
	return db.RepoState{
		Repo:     gb.RepoUrl(),
		Revision: getCommit(t, gb, "HEAD"),
	}
}

func makeTask(name, repo, revision string) *db.Task {
	return &db.Task{
		Commits: []string{revision},
		Created: time.Now(),
		TaskKey: db.TaskKey{
			RepoState: db.RepoState{
				Repo:     repo,
				Revision: revision,
			},
			Name: name,
		},
		SwarmingTaskId: "swarmid",
	}
}

func makeSwarmingRpcsTaskRequestMetadata(t *testing.T, task *db.Task, dims map[string]string) *swarming_api.SwarmingRpcsTaskRequestMetadata {
	tag := func(k, v string) string {
		return fmt.Sprintf("%s:%s", k, v)
	}
	ts := func(t time.Time) string {
		if util.TimeIsZero(t) {
			return ""
		}
		return t.Format(swarming.TIMESTAMP_FORMAT)
	}
	abandoned := ""
	state := swarming.TASK_STATE_PENDING
	failed := false
	switch task.Status {
	case db.TASK_STATUS_MISHAP:
		state = swarming.TASK_STATE_BOT_DIED
		abandoned = ts(task.Finished)
	case db.TASK_STATUS_RUNNING:
		state = swarming.TASK_STATE_RUNNING
	case db.TASK_STATUS_FAILURE:
		state = swarming.TASK_STATE_COMPLETED
		failed = true
	case db.TASK_STATUS_SUCCESS:
		state = swarming.TASK_STATE_COMPLETED
	case db.TASK_STATUS_PENDING:
		// noop
	default:
		assert.FailNow(t, "Unknown task status: %s", task.Status)
	}
	tags := []string{
		tag(db.SWARMING_TAG_ID, task.Id),
		tag(db.SWARMING_TAG_FORCED_JOB_ID, task.ForcedJobId),
		tag(db.SWARMING_TAG_NAME, task.Name),
		tag(swarming.DIMENSION_POOL_KEY, swarming.DIMENSION_POOL_VALUE_SKIA),
		tag(db.SWARMING_TAG_REPO, task.Repo),
		tag(db.SWARMING_TAG_REVISION, task.Revision),
	}
	for _, p := range task.ParentTaskIds {
		tags = append(tags, tag(db.SWARMING_TAG_PARENT_TASK_ID, p))
	}

	dimensions := make([]*swarming_api.SwarmingRpcsStringPair, 0, len(dims))
	for k, v := range dims {
		dimensions = append(dimensions, &swarming_api.SwarmingRpcsStringPair{
			Key:   k,
			Value: v,
		})
	}

	return &swarming_api.SwarmingRpcsTaskRequestMetadata{
		Request: &swarming_api.SwarmingRpcsTaskRequest{
			CreatedTs: ts(task.Created),
			Properties: &swarming_api.SwarmingRpcsTaskProperties{
				Dimensions: dimensions,
			},
			Tags: tags,
		},
		TaskId: task.SwarmingTaskId,
		TaskResult: &swarming_api.SwarmingRpcsTaskResult{
			AbandonedTs: abandoned,
			BotId:       task.SwarmingBotId,
			CreatedTs:   ts(task.Created),
			CompletedTs: ts(task.Finished),
			Failure:     failed,
			OutputsRef: &swarming_api.SwarmingRpcsFilesRef{
				Isolated: task.IsolatedOutput,
			},
			StartedTs: ts(task.Started),
			State:     state,
			Tags:      tags,
			TaskId:    task.SwarmingTaskId,
		},
	}
}

// Common setup for TaskScheduler tests.
func setup(t *testing.T) (*git_testutils.GitBuilder, db.DB, *swarming.TestClient, *TaskScheduler, *mockhttpclient.URLMock, func()) {
	testutils.LargeTest(t)
	testutils.SkipIfShort(t)

	gb, _, _ := specs_testutils.SetupTestRepo(t)

	tmp, err := ioutil.TempDir("", "")
	assert.NoError(t, err)

	assert.NoError(t, os.Mkdir(path.Join(tmp, TRIGGER_DIRNAME), os.ModePerm))
	d := db.NewInMemoryDB()
	isolateClient, err := isolate.NewClient(tmp, isolate.ISOLATE_SERVER_URL_FAKE)
	assert.NoError(t, err)
	swarmingClient := swarming.NewTestClient()
	urlMock := mockhttpclient.NewURLMock()
	repos, err := repograph.NewMap([]string{gb.RepoUrl()}, tmp)
	assert.NoError(t, err)
	projectRepoMapping := map[string]string{
		"skia": gb.RepoUrl(),
	}
	depotTools := depot_tools_testutils.GetDepotTools(t)
	gitcookies := path.Join(tmp, "gitcookies_fake")
	assert.NoError(t, ioutil.WriteFile(gitcookies, []byte(".googlesource.com\tTRUE\t/\tTRUE\t123\to\tgit-user.google.com=abc123"), os.ModePerm))
	g, err := gerrit.NewGerrit(fakeGerritUrl, gitcookies, urlMock.Client())
	assert.NoError(t, err)
	s, err := NewTaskScheduler(d, time.Duration(math.MaxInt64), 0, tmp, "fake.server", repos, isolateClient, swarmingClient, urlMock.Client(), 1.0, tryjobs.API_URL_TESTING, tryjobs.BUCKET_TESTING, projectRepoMapping, swarming.POOLS_PUBLIC, "", depotTools, g)
	assert.NoError(t, err)
	return gb, d, swarmingClient, s, urlMock, func() {
		testutils.RemoveAll(t, tmp)
		gb.Cleanup()
	}
}

func TestGatherNewJobs(t *testing.T) {
	gb, _, _, s, _, cleanup := setup(t)
	defer cleanup()

	testGatherNewJobs := func(expectedJobs int) {
		assert.NoError(t, s.gatherNewJobs())
		jobs, err := s.jCache.UnfinishedJobs()
		assert.NoError(t, err)
		assert.Equal(t, expectedJobs, len(jobs))
	}

	// Ensure that the JobDB is empty.
	jobs, err := s.jCache.UnfinishedJobs()
	assert.NoError(t, err)
	assert.Equal(t, 0, len(jobs))

	// Run gatherNewJobs, ensure that we added jobs for all commits in the
	// repo.
	testGatherNewJobs(5) // c1 has 2 jobs, c2 has 3 jobs.

	// Run gatherNewJobs again, ensure that we didn't add the same Jobs
	// again.
	testGatherNewJobs(5) // no new jobs == 5 total jobs.

	// Add a commit on master, run gatherNewJobs, ensure that we added the
	// new Jobs.
	makeDummyCommits(gb, 1)
	assert.NoError(t, s.updateRepos())
	testGatherNewJobs(8) // we didn't add to the jobs spec, so 3 jobs/rev.

	// Add several commits on master, ensure that we added all of the Jobs.
	makeDummyCommits(gb, 10)
	assert.NoError(t, s.updateRepos())
	testGatherNewJobs(38) // 3 jobs/rev + 8 pre-existing jobs.

	// Add a commit on a branch other than master, run gatherNewJobs, ensure
	// that we added the new Jobs.
	branchName := "otherBranch"
	gb.CreateBranchTrackBranch(branchName, "master")
	msg := "Branch commit"
	fileName := "some_other_file"
	gb.Add(fileName, msg)
	gb.Commit()
	assert.NoError(t, s.updateRepos())
	testGatherNewJobs(41) // 38 previous jobs + 3 new ones.

	// Add several commits in a row on different branches, ensure that we
	// added all of the Jobs for all of the new commits.
	makeDummyCommits(gb, 5)
	gb.CheckoutBranch("master")
	makeDummyCommits(gb, 5)
	assert.NoError(t, s.updateRepos())
	testGatherNewJobs(71) // 10 commits x 3 jobs/commit = 30, plus 41
}

func TestFindTaskCandidatesForJobs(t *testing.T) {
	gb, _, _, s, _, cleanup := setup(t)
	defer cleanup()

	test := func(jobs []*db.Job, expect map[db.TaskKey]*taskCandidate) {
		actual, err := s.findTaskCandidatesForJobs(jobs)
		assert.NoError(t, err)
		testutils.AssertDeepEqual(t, actual, expect)
	}

	rs1 := getRS1(t, gb)
	rs2 := getRS2(t, gb)

	// Get all of the task specs, for future use.
	ts, err := s.taskCfgCache.GetTaskSpecsForRepoStates([]db.RepoState{rs1, rs2})
	assert.NoError(t, err)

	// Run on an empty job list, ensure empty list returned.
	test([]*db.Job{}, map[db.TaskKey]*taskCandidate{})

	now := time.Now().UTC()

	// Run for one job, ensure that we get the right set of task specs
	// returned (ie. all dependencies and their dependencies).
	j1 := &db.Job{
		Created:      now,
		Name:         "j1",
		Dependencies: map[string][]string{specs_testutils.TestTask: {specs_testutils.BuildTask}, specs_testutils.BuildTask: {}},
		Priority:     0.5,
		RepoState:    rs1.Copy(),
	}
	tc1 := &taskCandidate{
		JobCreated: now,
		TaskKey: db.TaskKey{
			RepoState: rs1.Copy(),
			Name:      specs_testutils.BuildTask,
		},
		TaskSpec: ts[rs1][specs_testutils.BuildTask].Copy(),
	}
	tc2 := &taskCandidate{
		JobCreated: now,
		TaskKey: db.TaskKey{
			RepoState: rs1.Copy(),
			Name:      specs_testutils.TestTask,
		},
		TaskSpec: ts[rs1][specs_testutils.TestTask].Copy(),
	}

	test([]*db.Job{j1}, map[db.TaskKey]*taskCandidate{
		tc1.TaskKey: tc1,
		tc2.TaskKey: tc2,
	})

	// Add a job, ensure that its dependencies are added and that the right
	// dependencies are de-duplicated.
	j2 := &db.Job{
		Created:      now,
		Name:         "j2",
		Dependencies: map[string][]string{specs_testutils.TestTask: {specs_testutils.BuildTask}, specs_testutils.BuildTask: {}},
		Priority:     0.6,
		RepoState:    rs2,
	}
	j3 := &db.Job{
		Created:      now,
		Name:         "j3",
		Dependencies: map[string][]string{specs_testutils.PerfTask: {specs_testutils.BuildTask}, specs_testutils.BuildTask: {}},
		Priority:     0.6,
		RepoState:    rs2,
	}
	tc3 := &taskCandidate{
		JobCreated: now,
		TaskKey: db.TaskKey{
			RepoState: rs2.Copy(),
			Name:      specs_testutils.BuildTask,
		},
		TaskSpec: ts[rs2][specs_testutils.BuildTask].Copy(),
	}
	tc4 := &taskCandidate{
		JobCreated: now,
		TaskKey: db.TaskKey{
			RepoState: rs2.Copy(),
			Name:      specs_testutils.TestTask,
		},
		TaskSpec: ts[rs2][specs_testutils.TestTask].Copy(),
	}
	tc5 := &taskCandidate{
		JobCreated: now,
		TaskKey: db.TaskKey{
			RepoState: rs2.Copy(),
			Name:      specs_testutils.PerfTask,
		},
		TaskSpec: ts[rs2][specs_testutils.PerfTask].Copy(),
	}
	allCandidates := map[db.TaskKey]*taskCandidate{
		tc1.TaskKey: tc1,
		tc2.TaskKey: tc2,
		tc3.TaskKey: tc3,
		tc4.TaskKey: tc4,
		tc5.TaskKey: tc5,
	}
	test([]*db.Job{j1, j2, j3}, allCandidates)

	// Finish j3, ensure that its task specs no longer show up.
	delete(allCandidates, j3.MakeTaskKey(specs_testutils.PerfTask))
	test([]*db.Job{j1, j2}, allCandidates)
}

func TestFilterTaskCandidates(t *testing.T) {
	gb, d, _, s, _, cleanup := setup(t)
	defer cleanup()

	rs1 := getRS1(t, gb)
	rs2 := getRS2(t, gb)
	c1 := rs1.Revision
	c2 := rs2.Revision

	// Fake out the initial candidates.
	k1 := db.TaskKey{
		RepoState: rs1,
		Name:      specs_testutils.BuildTask,
	}
	k2 := db.TaskKey{
		RepoState: rs1,
		Name:      specs_testutils.TestTask,
	}
	k3 := db.TaskKey{
		RepoState: rs2,
		Name:      specs_testutils.BuildTask,
	}
	k4 := db.TaskKey{
		RepoState: rs2,
		Name:      specs_testutils.TestTask,
	}
	k5 := db.TaskKey{
		RepoState: rs2,
		Name:      specs_testutils.PerfTask,
	}
	candidates := map[db.TaskKey]*taskCandidate{
		k1: {
			TaskKey:  k1,
			TaskSpec: &specs.TaskSpec{},
		},
		k2: {
			TaskKey: k2,
			TaskSpec: &specs.TaskSpec{
				Dependencies: []string{specs_testutils.BuildTask},
			},
		},
		k3: {
			TaskKey:  k3,
			TaskSpec: &specs.TaskSpec{},
		},
		k4: {
			TaskKey: k4,
			TaskSpec: &specs.TaskSpec{
				Dependencies: []string{specs_testutils.BuildTask},
			},
		},
		k5: {
			TaskKey: k5,
			TaskSpec: &specs.TaskSpec{
				Dependencies: []string{specs_testutils.BuildTask},
			},
		},
	}

	// Check the initial set of task candidates. The two Build tasks
	// should be the only ones available.
	c, err := s.filterTaskCandidates(candidates)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(c))
	assert.Equal(t, 1, len(c[gb.RepoUrl()]))
	assert.Equal(t, 2, len(c[gb.RepoUrl()][specs_testutils.BuildTask]))
	for _, byRepo := range c {
		for _, byName := range byRepo {
			for _, candidate := range byName {
				assert.Equal(t, candidate.Name, specs_testutils.BuildTask)
			}
		}
	}

	// Insert a the Build task at c1 (1 dependent) into the database,
	// transition through various states.
	var t1 *db.Task
	for _, byRepo := range c { // Order not guaranteed, find the right candidate.
		for _, byName := range byRepo {
			for _, candidate := range byName {
				if candidate.Revision == c1 {
					t1 = makeTask(candidate.Name, candidate.Repo, candidate.Revision)
					break
				}
			}
		}
	}
	assert.NotNil(t, t1)

	// We shouldn't duplicate pending or running tasks.
	for _, status := range []db.TaskStatus{db.TASK_STATUS_PENDING, db.TASK_STATUS_RUNNING} {
		t1.Status = status
		assert.NoError(t, d.PutTask(t1))
		assert.NoError(t, s.tCache.Update())

		c, err = s.filterTaskCandidates(candidates)
		assert.NoError(t, err)
		assert.Equal(t, 1, len(c))
		for _, byRepo := range c {
			for _, byName := range byRepo {
				for _, candidate := range byName {
					assert.Equal(t, candidate.Name, specs_testutils.BuildTask)
					assert.Equal(t, c2, candidate.Revision)
				}
			}
		}
	}

	// The task failed. Ensure that its dependents are not candidates, but
	// the task itself is back in the list of candidates, in case we want
	// to retry.
	t1.Status = db.TASK_STATUS_FAILURE
	assert.NoError(t, d.PutTask(t1))
	assert.NoError(t, s.tCache.Update())

	c, err = s.filterTaskCandidates(candidates)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(c))
	for _, byRepo := range c {
		assert.Equal(t, 1, len(byRepo))
		for _, byName := range byRepo {
			assert.Equal(t, 2, len(byName))
			for _, candidate := range byName {
				assert.Equal(t, candidate.Name, specs_testutils.BuildTask)
			}
		}
	}

	// The task succeeded. Ensure that its dependents are candidates and
	// the task itself is not.
	t1.Status = db.TASK_STATUS_SUCCESS
	t1.IsolatedOutput = "fake isolated hash"
	assert.NoError(t, d.PutTask(t1))
	assert.NoError(t, s.tCache.Update())

	c, err = s.filterTaskCandidates(candidates)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(c))
	for _, byRepo := range c {
		assert.Equal(t, 2, len(byRepo))
		for _, byName := range byRepo {
			for _, candidate := range byName {
				assert.False(t, t1.Name == candidate.Name && t1.Revision == candidate.Revision)
			}
		}
	}

	// Create the other Build task.
	var t2 *db.Task
	for _, byRepo := range c {
		for _, byName := range byRepo {
			for _, candidate := range byName {
				if candidate.Revision == c2 && strings.HasPrefix(candidate.Name, "Build-") {
					t2 = makeTask(candidate.Name, candidate.Repo, candidate.Revision)
					break
				}
			}
		}
	}
	assert.NotNil(t, t2)
	t2.Status = db.TASK_STATUS_SUCCESS
	t2.IsolatedOutput = "fake isolated hash"
	assert.NoError(t, d.PutTask(t2))
	assert.NoError(t, s.tCache.Update())

	// All test and perf tasks are now candidates, no build tasks.
	c, err = s.filterTaskCandidates(candidates)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(c))
	assert.Equal(t, 2, len(c[gb.RepoUrl()][specs_testutils.TestTask]))
	assert.Equal(t, 1, len(c[gb.RepoUrl()][specs_testutils.PerfTask]))
	for _, byRepo := range c {
		for _, byName := range byRepo {
			for _, candidate := range byName {
				assert.NotEqual(t, candidate.Name, specs_testutils.BuildTask)
			}
		}
	}

	// Add a try job. Ensure that no deps have been incorrectly satisfied.
	tryKey := k4.Copy()
	tryKey.Server = "dummy-server"
	tryKey.Issue = "dummy-issue"
	tryKey.Patchset = "dummy-patchset"
	candidates[tryKey] = &taskCandidate{
		TaskKey: tryKey,
		TaskSpec: &specs.TaskSpec{
			Dependencies: []string{specs_testutils.BuildTask},
		},
	}
	c, err = s.filterTaskCandidates(candidates)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(c))
	assert.Equal(t, 2, len(c[gb.RepoUrl()][specs_testutils.TestTask]))
	assert.Equal(t, 1, len(c[gb.RepoUrl()][specs_testutils.PerfTask]))
	for _, byRepo := range c {
		for _, byName := range byRepo {
			for _, candidate := range byName {
				assert.NotEqual(t, candidate.Name, specs_testutils.BuildTask)
				assert.False(t, candidate.IsTryJob())
			}
		}
	}
}

func TestProcessTaskCandidate(t *testing.T) {
	gb, _, _, s, _, cleanup := setup(t)
	defer cleanup()

	c1 := getRS1(t, gb).Revision
	c2 := getRS2(t, gb).Revision

	cache := newCacheWrapper(s.tCache)
	now := time.Unix(0, 1470674884000000)
	commitsBuf := make([]*repograph.Commit, 0, MAX_BLAMELIST_COMMITS)

	// Try job candidates have a specific score and no blamelist.
	c := &taskCandidate{
		JobCreated: now.Add(-1 * time.Hour),
		TaskKey: db.TaskKey{
			RepoState: db.RepoState{
				Patch: db.Patch{
					Server:   "my-server",
					Issue:    "my-issue",
					Patchset: "my-patchset",
				},
				Repo:     gb.RepoUrl(),
				Revision: c1,
			},
		},
	}
	assert.NoError(t, s.processTaskCandidate(c, now, cache, commitsBuf))
	assert.Equal(t, CANDIDATE_SCORE_TRY_JOB+1.0, c.Score)
	assert.Nil(t, c.Commits)

	// Manually forced candidates have a blamelist and a specific score.
	c = &taskCandidate{
		JobCreated: now.Add(-2 * time.Hour),
		TaskKey: db.TaskKey{
			RepoState: db.RepoState{
				Repo:     gb.RepoUrl(),
				Revision: c2,
			},
			ForcedJobId: "my-job",
		},
	}
	assert.NoError(t, s.processTaskCandidate(c, now, cache, commitsBuf))
	assert.Equal(t, CANDIDATE_SCORE_FORCE_RUN+2.0, c.Score)
	assert.Equal(t, 2, len(c.Commits))

	// All other candidates have a blamelist and a time-decayed score.
	c = &taskCandidate{
		TaskKey: db.TaskKey{
			RepoState: db.RepoState{
				Repo:     gb.RepoUrl(),
				Revision: c2,
			},
		},
	}
	assert.NoError(t, s.processTaskCandidate(c, now, cache, commitsBuf))
	assert.True(t, c.Score > 0)
	assert.Equal(t, 2, len(c.Commits))

	// Now, replace the time window to ensure that this next candidate runs
	// at a commit outside the window. Ensure that it gets the correct
	// blamelist.
	var err error
	s.window, err = window.New(time.Nanosecond, 0, nil)
	assert.NoError(t, err)
	c = &taskCandidate{
		TaskKey: db.TaskKey{
			RepoState: db.RepoState{
				Repo:     gb.RepoUrl(),
				Revision: c2,
			},
		},
	}
	assert.NoError(t, s.processTaskCandidate(c, now, cache, commitsBuf))
	assert.Equal(t, 0, len(c.Commits))
}

func TestProcessTaskCandidates(t *testing.T) {
	gb, _, _, s, _, cleanup := setup(t)
	defer cleanup()

	ts := time.Now()

	rs1 := getRS1(t, gb)
	rs2 := getRS2(t, gb)

	// Processing of individual candidates is already tested; just verify
	// that if we pass in a bunch of candidates they all get processed.
	assertProcessed := func(c *taskCandidate) {
		if c.IsTryJob() {
			assert.True(t, c.Score > CANDIDATE_SCORE_TRY_JOB)
			assert.Nil(t, c.Commits)
		} else if c.IsForceRun() {
			assert.True(t, c.Score > CANDIDATE_SCORE_FORCE_RUN)
			assert.Equal(t, 2, len(c.Commits))
		} else if c.Revision == rs2.Revision {
			if c.Name == specs_testutils.PerfTask {
				assert.Equal(t, 2.0, c.Score)
				assert.Equal(t, 1, len(c.Commits))
			} else if c.Name == specs_testutils.BuildTask {
				assert.Equal(t, 0.0, c.Score) // Already covered by the forced job.
				assert.Equal(t, 1, len(c.Commits))
			} else {
				assert.Equal(t, 3.5, c.Score)
				assert.Equal(t, 2, len(c.Commits))
			}
		} else {
			assert.Equal(t, 0.5, c.Score) // These will be backfills.
			assert.Equal(t, 1, len(c.Commits))
		}
	}

	candidates := map[string]map[string][]*taskCandidate{
		gb.RepoUrl(): {
			specs_testutils.BuildTask: {
				{
					JobCreated: ts,
					TaskKey: db.TaskKey{
						RepoState: rs1,
						Name:      specs_testutils.BuildTask,
					},
					TaskSpec: &specs.TaskSpec{},
				},
				{
					JobCreated: ts,
					TaskKey: db.TaskKey{
						RepoState: rs2,
						Name:      specs_testutils.BuildTask,
					},
					TaskSpec: &specs.TaskSpec{},
				},
				{
					JobCreated: ts,
					TaskKey: db.TaskKey{
						RepoState:   rs2,
						Name:        specs_testutils.BuildTask,
						ForcedJobId: "my-job",
					},
					TaskSpec: &specs.TaskSpec{},
				},
			},
			specs_testutils.TestTask: {
				{
					JobCreated: ts,
					TaskKey: db.TaskKey{
						RepoState: rs1,
						Name:      specs_testutils.TestTask,
					},
					TaskSpec: &specs.TaskSpec{},
				},
				{
					JobCreated: ts,
					TaskKey: db.TaskKey{
						RepoState: rs2,
						Name:      specs_testutils.TestTask,
					},
					TaskSpec: &specs.TaskSpec{},
				},
			},
			specs_testutils.PerfTask: {
				{
					JobCreated: ts,
					TaskKey: db.TaskKey{
						RepoState: rs2,
						Name:      specs_testutils.PerfTask,
					},
					TaskSpec: &specs.TaskSpec{},
				},
				{
					JobCreated: ts,
					TaskKey: db.TaskKey{
						RepoState: db.RepoState{
							Patch: db.Patch{
								Server:   "my-server",
								Issue:    "my-issue",
								Patchset: "my-patchset",
							},
							Repo:     gb.RepoUrl(),
							Revision: rs1.Revision,
						},
						Name: specs_testutils.PerfTask,
					},
					TaskSpec: &specs.TaskSpec{},
				},
			},
		},
	}

	processed, err := s.processTaskCandidates(candidates, time.Now())
	assert.NoError(t, err)
	for _, c := range processed {
		assertProcessed(c)
	}
	assert.Equal(t, 7, len(processed))
}

func TestTestedness(t *testing.T) {
	testutils.SmallTest(t)
	tc := []struct {
		in  int
		out float64
	}{
		{
			in:  -1,
			out: -1.0,
		},
		{
			in:  0,
			out: 0.0,
		},
		{
			in:  1,
			out: 1.0,
		},
		{
			in:  2,
			out: 1.0 + 1.0/2.0,
		},
		{
			in:  3,
			out: 1.0 + float64(2.0)/float64(3.0),
		},
		{
			in:  4,
			out: 1.0 + 3.0/4.0,
		},
		{
			in:  4096,
			out: 1.0 + float64(4095)/float64(4096),
		},
	}
	for i, c := range tc {
		assert.Equal(t, c.out, testedness(c.in), fmt.Sprintf("test case #%d", i))
	}
}

func TestTestednessIncrease(t *testing.T) {
	testutils.SmallTest(t)
	tc := []struct {
		a   int
		b   int
		out float64
	}{
		// Invalid cases.
		{
			a:   -1,
			b:   10,
			out: -1.0,
		},
		{
			a:   10,
			b:   -1,
			out: -1.0,
		},
		{
			a:   0,
			b:   -1,
			out: -1.0,
		},
		{
			a:   0,
			b:   0,
			out: -1.0,
		},
		// Invalid because if we're re-running at already-tested commits
		// then we should have a blamelist which is at most the size of
		// the blamelist of the previous task. We naturally get negative
		// testedness increase in these cases.
		{
			a:   2,
			b:   1,
			out: -0.5,
		},
		// Testing only new commits.
		{
			a:   1,
			b:   0,
			out: 1.0 + 1.0,
		},
		{
			a:   2,
			b:   0,
			out: 2.0 + (1.0 + 1.0/2.0),
		},
		{
			a:   3,
			b:   0,
			out: 3.0 + (1.0 + float64(2.0)/float64(3.0)),
		},
		{
			a:   4096,
			b:   0,
			out: 4096.0 + (1.0 + float64(4095.0)/float64(4096.0)),
		},
		// Retries.
		{
			a:   1,
			b:   1,
			out: 0.0,
		},
		{
			a:   2,
			b:   2,
			out: 0.0,
		},
		{
			a:   3,
			b:   3,
			out: 0.0,
		},
		{
			a:   4096,
			b:   4096,
			out: 0.0,
		},
		// Bisect/backfills.
		{
			a:   1,
			b:   2,
			out: 0.5, // (1 + 1) - (1 + 1/2)
		},
		{
			a:   1,
			b:   3,
			out: float64(2.5) - (1.0 + float64(2.0)/float64(3.0)),
		},
		{
			a:   5,
			b:   10,
			out: 2.0*(1.0+float64(4.0)/float64(5.0)) - (1.0 + float64(9.0)/float64(10.0)),
		},
	}
	for i, c := range tc {
		assert.Equal(t, c.out, testednessIncrease(c.a, c.b), fmt.Sprintf("test case #%d", i))
	}
}

func TestComputeBlamelist(t *testing.T) {
	testutils.LargeTest(t)
	testutils.SkipIfShort(t)

	// Setup.
	gb := git_testutils.GitInit(t)
	defer gb.Cleanup()

	tmp, err := ioutil.TempDir("", "")
	assert.NoError(t, err)
	defer testutils.RemoveAll(t, tmp)

	d := db.NewInMemoryTaskDB()
	w, err := window.New(time.Hour, 0, nil)
	cache, err := db.NewTaskCache(d, w)
	assert.NoError(t, err)

	// The test repo is laid out like this:
	//
	// *   O (HEAD, master, Case #9)
	// *   N
	// *   M (Case #10)
	// *   L
	// *   K (Case #6)
	// *   J (Case #5)
	// |\
	// | * I
	// | * H (Case #4)
	// * | G
	// * | F (Case #3)
	// * | E (Case #8, previously #7)
	// |/
	// *   D (Case #2)
	// *   C (Case #1)
	// ...
	// *   B (Case #0)
	// *   A
	//
	hashes := map[string]string{}
	commit := func(file, name string) {
		hashes[name] = gb.CommitGenMsg(file, name)
	}

	// Initial commit.
	f := "somefile"
	f2 := "file2"
	commit(f, "A")

	type testCase struct {
		Revision     string
		Expected     []string
		StoleFromIdx int
	}

	name := "Test-Ubuntu12-ShuttleA-GTX660-x86-Release"

	repos, err := repograph.NewMap([]string{gb.RepoUrl()}, tmp)
	assert.NoError(t, err)
	repo := repos[gb.RepoUrl()]
	depotTools := depot_tools_testutils.GetDepotTools(t)
	tcc, err := specs.NewTaskCfgCache(repos, depotTools, tmp, 1)
	assert.NoError(t, err)

	ids := []string{}
	commitsBuf := make([]*repograph.Commit, 0, MAX_BLAMELIST_COMMITS)
	test := func(tc *testCase) {
		// Update the repo.
		assert.NoError(t, repo.Update())
		// Self-check: make sure we don't pass in empty commit hashes.
		for _, h := range tc.Expected {
			assert.NotEqual(t, h, "")
		}

		newTasks, err := tcc.GetAddedTaskSpecsForRepoStates([]db.RepoState{
			{
				Repo:     gb.RepoUrl(),
				Revision: tc.Revision,
			},
		})
		assert.NoError(t, err)

		// Ensure that we get the expected blamelist.
		revision := repo.Get(tc.Revision)
		assert.NotNil(t, revision)
		commits, stoleFrom, err := ComputeBlamelist(cache, repo, name, gb.RepoUrl(), revision, commitsBuf, newTasks)
		if tc.Revision == "" {
			assert.Error(t, err)
			return
		} else {
			assert.NoError(t, err)
		}
		sort.Strings(commits)
		sort.Strings(tc.Expected)
		testutils.AssertDeepEqual(t, tc.Expected, commits)
		if tc.StoleFromIdx >= 0 {
			assert.NotNil(t, stoleFrom)
			assert.Equal(t, ids[tc.StoleFromIdx], stoleFrom.Id)
		} else {
			assert.Nil(t, stoleFrom)
		}

		// Insert the task into the DB.
		c := &taskCandidate{
			TaskKey: db.TaskKey{
				RepoState: db.RepoState{
					Repo:     gb.RepoUrl(),
					Revision: tc.Revision,
				},
				Name: name,
			},
			TaskSpec: &specs.TaskSpec{},
		}
		task := c.MakeTask()
		task.Commits = commits
		task.Created = time.Now()
		if stoleFrom != nil {
			// Re-insert the stoleFrom task without the commits
			// which were stolen from it.
			stoleFromCommits := make([]string, 0, len(stoleFrom.Commits)-len(commits))
			for _, commit := range stoleFrom.Commits {
				if !util.In(commit, task.Commits) {
					stoleFromCommits = append(stoleFromCommits, commit)
				}
			}
			stoleFrom.Commits = stoleFromCommits
			assert.NoError(t, d.PutTasks([]*db.Task{task, stoleFrom}))
		} else {
			assert.NoError(t, d.PutTask(task))
		}
		ids = append(ids, task.Id)
		assert.NoError(t, cache.Update())
	}

	// Commit B.
	commit(f, "B")

	// Test cases. Each test case builds on the previous cases.

	// 0. The first task, at HEAD.
	test(&testCase{
		Revision:     hashes["B"],
		Expected:     []string{hashes["B"], hashes["A"]},
		StoleFromIdx: -1,
	})

	// Test the blamelist too long case by creating a bunch of commits.
	for i := 0; i < MAX_BLAMELIST_COMMITS+1; i++ {
		commit(f, "C")
	}
	commit(f, "D")

	// 1. Blamelist too long, not a branch head.
	test(&testCase{
		Revision:     hashes["C"],
		Expected:     []string{hashes["C"]},
		StoleFromIdx: -1,
	})

	// 2. Blamelist too long, is a branch head.
	test(&testCase{
		Revision:     hashes["D"],
		Expected:     []string{hashes["D"]},
		StoleFromIdx: -1,
	})

	// Create the remaining commits.
	gb.CreateBranchTrackBranch("otherbranch", "master")
	gb.CheckoutBranch("master")
	commit(f, "E")
	commit(f, "F")
	commit(f, "G")
	gb.CheckoutBranch("otherbranch")
	commit(f2, "H")
	commit(f2, "I")
	gb.CheckoutBranch("master")
	hashes["J"] = gb.MergeBranch("otherbranch")
	commit(f, "K")

	// 3. On a linear set of commits, with at least one previous task.
	test(&testCase{
		Revision:     hashes["F"],
		Expected:     []string{hashes["E"], hashes["F"]},
		StoleFromIdx: -1,
	})
	// 4. The first task on a new branch.
	test(&testCase{
		Revision:     hashes["H"],
		Expected:     []string{hashes["H"]},
		StoleFromIdx: -1,
	})
	// 5. After a merge.
	test(&testCase{
		Revision:     hashes["J"],
		Expected:     []string{hashes["G"], hashes["I"], hashes["J"]},
		StoleFromIdx: -1,
	})
	// 6. One last "normal" task.
	test(&testCase{
		Revision:     hashes["K"],
		Expected:     []string{hashes["K"]},
		StoleFromIdx: -1,
	})
	// 7. Steal commits from a previously-ingested task.
	test(&testCase{
		Revision:     hashes["E"],
		Expected:     []string{hashes["E"]},
		StoleFromIdx: 3,
	})

	// Ensure that task #8 really stole the commit from #3.
	task, err := cache.GetTask(ids[3])
	assert.NoError(t, err)
	assert.False(t, util.In(hashes["E"], task.Commits), fmt.Sprintf("Expected not to find %s in %v", hashes["E"], task.Commits))

	// 8. Retry #7.
	test(&testCase{
		Revision:     hashes["E"],
		Expected:     []string{hashes["E"]},
		StoleFromIdx: 7,
	})

	// Ensure that task #8 really stole the commit from #7.
	task, err = cache.GetTask(ids[7])
	assert.NoError(t, err)
	assert.Equal(t, 0, len(task.Commits))

	// Four more commits.
	commit(f, "L")
	commit(f, "M")
	commit(f, "N")
	commit(f, "O")

	// 9. Not really a test case, but setting up for #10.
	test(&testCase{
		Revision:     hashes["O"],
		Expected:     []string{hashes["L"], hashes["M"], hashes["N"], hashes["O"]},
		StoleFromIdx: -1,
	})

	// 10. Steal *two* commits from #9.
	test(&testCase{
		Revision:     hashes["M"],
		Expected:     []string{hashes["L"], hashes["M"]},
		StoleFromIdx: 9,
	})
}

func TestTimeDecay24Hr(t *testing.T) {
	testutils.SmallTest(t)
	tc := []struct {
		decayAmt24Hr float64
		elapsed      time.Duration
		out          float64
	}{
		{
			decayAmt24Hr: 1.0,
			elapsed:      10 * time.Hour,
			out:          1.0,
		},
		{
			decayAmt24Hr: 0.5,
			elapsed:      0 * time.Hour,
			out:          1.0,
		},
		{
			decayAmt24Hr: 0.5,
			elapsed:      24 * time.Hour,
			out:          0.5,
		},
		{
			decayAmt24Hr: 0.5,
			elapsed:      12 * time.Hour,
			out:          0.75,
		},
		{
			decayAmt24Hr: 0.5,
			elapsed:      36 * time.Hour,
			out:          0.25,
		},
		{
			decayAmt24Hr: 0.5,
			elapsed:      48 * time.Hour,
			out:          0.0,
		},
		{
			decayAmt24Hr: 0.5,
			elapsed:      72 * time.Hour,
			out:          0.0,
		},
	}
	for i, c := range tc {
		assert.Equal(t, c.out, timeDecay24Hr(c.decayAmt24Hr, c.elapsed), fmt.Sprintf("test case #%d", i))
	}
}

func TestRegenerateTaskQueue(t *testing.T) {
	gb, d, _, s, _, cleanup := setup(t)
	defer cleanup()

	// Ensure that the queue is initially empty.
	assert.Equal(t, 0, len(s.queue))

	rs1 := getRS1(t, gb)
	rs2 := getRS2(t, gb)
	c1 := rs1.Revision
	c2 := rs2.Revision

	// Our test repo has a job pointing to every task.
	now := time.Now()
	j1 := &db.Job{
		Created:      now,
		Name:         "j1",
		Dependencies: map[string][]string{specs_testutils.BuildTask: {}},
		Priority:     0.5,
		RepoState:    rs1.Copy(),
	}
	j2 := &db.Job{
		Created:      now,
		Name:         "j2",
		Dependencies: map[string][]string{specs_testutils.TestTask: {specs_testutils.BuildTask}, specs_testutils.BuildTask: {}},
		Priority:     0.5,
		RepoState:    rs1.Copy(),
	}
	j3 := &db.Job{
		Created:      now,
		Name:         "j3",
		Dependencies: map[string][]string{specs_testutils.BuildTask: {}},
		Priority:     0.5,
		RepoState:    rs2.Copy(),
	}
	j4 := &db.Job{
		Created:      now,
		Name:         "j4",
		Dependencies: map[string][]string{specs_testutils.TestTask: {specs_testutils.BuildTask}, specs_testutils.BuildTask: {}},
		Priority:     0.5,
		RepoState:    rs2.Copy(),
	}
	j5 := &db.Job{
		Created:      now,
		Name:         "j5",
		Dependencies: map[string][]string{specs_testutils.PerfTask: {specs_testutils.BuildTask}, specs_testutils.BuildTask: {}},
		Priority:     0.5,
		RepoState:    rs2.Copy(),
	}
	assert.NoError(t, d.PutJobs([]*db.Job{j1, j2, j3, j4, j5}))
	assert.NoError(t, s.tCache.Update())
	assert.NoError(t, s.jCache.Update())

	// Regenerate the task queue.
	queue, err := s.regenerateTaskQueue(time.Now())
	assert.NoError(t, err)
	assert.Equal(t, 2, len(queue)) // Two Build tasks.

	testSort := func() {
		// Ensure that we sorted correctly.
		if len(queue) == 0 {
			return
		}
		highScore := queue[0].Score
		for _, c := range queue {
			assert.True(t, highScore >= c.Score)
			highScore = c.Score
		}
	}
	testSort()

	// Since we haven't run any task yet, we should have the two Build
	// tasks. The one at HEAD should have a two-commit blamelist and a
	// score of 3.5. The other should have one commit in its blamelist and
	// a score of 0.5.
	assert.Equal(t, specs_testutils.BuildTask, queue[0].Name)
	assert.Equal(t, []string{c2, c1}, queue[0].Commits)
	assert.Equal(t, 3.5, queue[0].Score)
	assert.Equal(t, specs_testutils.BuildTask, queue[1].Name)
	assert.Equal(t, []string{c1}, queue[1].Commits)
	assert.Equal(t, 0.5, queue[1].Score)

	// Insert the task at c1, even though it scored lower.
	t1 := makeTask(queue[1].Name, queue[1].Repo, queue[1].Revision)
	assert.NotNil(t, t1)
	t1.Status = db.TASK_STATUS_SUCCESS
	t1.IsolatedOutput = "fake isolated hash"
	assert.NoError(t, d.PutTask(t1))
	assert.NoError(t, s.tCache.Update())

	// Regenerate the task queue.
	queue, err = s.regenerateTaskQueue(time.Now())
	assert.NoError(t, err)

	// Now we expect the queue to contain the other Build task and the one
	// Test task we unblocked by running the first Build task.
	assert.Equal(t, 2, len(queue))
	testSort()
	for _, c := range queue {
		if c.Name == specs_testutils.TestTask {
			assert.Equal(t, 2.0, c.Score)
			assert.Equal(t, 1, len(c.Commits))
		} else {
			assert.Equal(t, c.Name, specs_testutils.BuildTask)
			assert.Equal(t, 2.0, c.Score)
			assert.Equal(t, []string{c.Revision}, c.Commits)
		}
	}
	buildIdx := 0
	testIdx := 1
	if queue[1].Name == specs_testutils.BuildTask {
		buildIdx = 1
		testIdx = 0
	}
	assert.Equal(t, specs_testutils.BuildTask, queue[buildIdx].Name)
	assert.Equal(t, c2, queue[buildIdx].Revision)

	assert.Equal(t, specs_testutils.TestTask, queue[testIdx].Name)
	assert.Equal(t, c1, queue[testIdx].Revision)

	// Run the other Build task.
	t2 := makeTask(queue[buildIdx].Name, queue[buildIdx].Repo, queue[buildIdx].Revision)
	t2.Status = db.TASK_STATUS_SUCCESS
	t2.IsolatedOutput = "fake isolated hash"
	assert.NoError(t, d.PutTask(t2))
	assert.NoError(t, s.tCache.Update())

	// Regenerate the task queue.
	queue, err = s.regenerateTaskQueue(time.Now())
	assert.NoError(t, err)
	assert.Equal(t, 3, len(queue))
	testSort()
	perfIdx := -1
	for i, c := range queue {
		if c.Name == specs_testutils.PerfTask {
			perfIdx = i
			assert.Equal(t, c2, c.Revision)
			assert.Equal(t, 2.0, c.Score)
			assert.Equal(t, []string{c.Revision}, c.Commits)
		} else {
			assert.Equal(t, c.Name, specs_testutils.TestTask)
			if c.Revision == c2 {
				assert.Equal(t, 3.5, c.Score)
				assert.Equal(t, []string{c2, c1}, c.Commits)
			} else {
				assert.Equal(t, 0.5, c.Score)
				assert.Equal(t, []string{c.Revision}, c.Commits)
			}
		}
	}
	assert.True(t, perfIdx > -1)

	// Run the Test task at tip of tree; its blamelist covers both commits.
	t3 := makeTask(specs_testutils.TestTask, gb.RepoUrl(), c2)
	t3.Commits = []string{c2, c1}
	t3.Status = db.TASK_STATUS_SUCCESS
	t3.IsolatedOutput = "fake isolated hash"
	assert.NoError(t, d.PutTask(t3))
	assert.NoError(t, s.tCache.Update())

	// Regenerate the task queue.
	queue, err = s.regenerateTaskQueue(time.Now())
	assert.NoError(t, err)

	// Now we expect the queue to contain one Test and one Perf task. The
	// Test task is a backfill, and should have a score of 0.5.
	assert.Equal(t, 2, len(queue))
	testSort()
	// First candidate should be the perf task.
	assert.Equal(t, specs_testutils.PerfTask, queue[0].Name)
	assert.Equal(t, 2.0, queue[0].Score)
	// The test task is next, a backfill.
	assert.Equal(t, specs_testutils.TestTask, queue[1].Name)
	assert.Equal(t, 0.5, queue[1].Score)
}

func makeTaskCandidate(name string, dims []string) *taskCandidate {
	return &taskCandidate{
		Score: 1.0,
		TaskKey: db.TaskKey{
			Name: name,
		},
		TaskSpec: &specs.TaskSpec{
			Dimensions: dims,
		},
	}
}

func makeSwarmingBot(id string, dims []string) *swarming_api.SwarmingRpcsBotInfo {
	d := make([]*swarming_api.SwarmingRpcsStringListPair, 0, len(dims))
	for _, s := range dims {
		split := strings.SplitN(s, ":", 2)
		d = append(d, &swarming_api.SwarmingRpcsStringListPair{
			Key:   split[0],
			Value: []string{split[1]},
		})
	}
	return &swarming_api.SwarmingRpcsBotInfo{
		BotId:      id,
		Dimensions: d,
	}
}

func TestGetCandidatesToSchedule(t *testing.T) {
	testutils.MediumTest(t)
	// Empty lists.
	rv := getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{}, []*taskCandidate{})
	assert.Equal(t, 0, len(rv))

	t1 := makeTaskCandidate("task1", []string{"k:v"})
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{}, []*taskCandidate{t1})
	assert.Equal(t, 0, len(rv))

	b1 := makeSwarmingBot("bot1", []string{"k:v"})
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1}, []*taskCandidate{})
	assert.Equal(t, 0, len(rv))

	// Single match.
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1}, []*taskCandidate{t1})
	testutils.AssertDeepEqual(t, []*taskCandidate{t1}, rv)

	// No match.
	t1.TaskSpec.Dimensions[0] = "k:v2"
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1}, []*taskCandidate{t1})
	assert.Equal(t, 0, len(rv))

	// Add a task candidate to match b1.
	t1 = makeTaskCandidate("task1", []string{"k:v2"})
	t2 := makeTaskCandidate("task2", []string{"k:v"})
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1}, []*taskCandidate{t1, t2})
	testutils.AssertDeepEqual(t, []*taskCandidate{t2}, rv)

	// Switch the task order.
	t1 = makeTaskCandidate("task1", []string{"k:v2"})
	t2 = makeTaskCandidate("task2", []string{"k:v"})
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1}, []*taskCandidate{t2, t1})
	testutils.AssertDeepEqual(t, []*taskCandidate{t2}, rv)

	// Make both tasks match the bot, ensure that we pick the first one.
	t1 = makeTaskCandidate("task1", []string{"k:v"})
	t2 = makeTaskCandidate("task2", []string{"k:v"})
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1}, []*taskCandidate{t1, t2})
	testutils.AssertDeepEqual(t, []*taskCandidate{t1}, rv)
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1}, []*taskCandidate{t2, t1})
	testutils.AssertDeepEqual(t, []*taskCandidate{t2}, rv)

	// Multiple dimensions. Ensure that different permutations of the bots
	// and tasks lists give us the expected results.
	dims := []string{"k:v", "k2:v2", "k3:v3"}
	b1 = makeSwarmingBot("bot1", dims)
	b2 := makeSwarmingBot("bot2", t1.TaskSpec.Dimensions)
	t1 = makeTaskCandidate("task1", []string{"k:v"})
	t2 = makeTaskCandidate("task2", dims)
	// In the first two cases, the task with fewer dimensions has the
	// higher priority. It gets the bot with more dimensions because it
	// is first in sorted order. The second task does not get scheduled
	// because there is no bot available which can run it.
	// TODO(borenet): Use a more optimal solution to avoid this case.
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1, b2}, []*taskCandidate{t1, t2})
	testutils.AssertDeepEqual(t, []*taskCandidate{t1}, rv)
	t1 = makeTaskCandidate("task1", []string{"k:v"})
	t2 = makeTaskCandidate("task2", dims)
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b2, b1}, []*taskCandidate{t1, t2})
	testutils.AssertDeepEqual(t, []*taskCandidate{t1}, rv)
	// In these two cases, the task with more dimensions has the higher
	// priority. Both tasks get scheduled.
	t1 = makeTaskCandidate("task1", []string{"k:v"})
	t2 = makeTaskCandidate("task2", dims)
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1, b2}, []*taskCandidate{t2, t1})
	testutils.AssertDeepEqual(t, []*taskCandidate{t2, t1}, rv)
	t1 = makeTaskCandidate("task1", []string{"k:v"})
	t2 = makeTaskCandidate("task2", dims)
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b2, b1}, []*taskCandidate{t2, t1})
	testutils.AssertDeepEqual(t, []*taskCandidate{t2, t1}, rv)

	// Matching dimensions. More bots than tasks.
	b2 = makeSwarmingBot("bot2", dims)
	b3 := makeSwarmingBot("bot3", dims)
	t1 = makeTaskCandidate("task1", dims)
	t2 = makeTaskCandidate("task2", dims)
	t3 := makeTaskCandidate("task3", dims)
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1, b2, b3}, []*taskCandidate{t1, t2})
	testutils.AssertDeepEqual(t, []*taskCandidate{t1, t2}, rv)

	// More tasks than bots.
	t1 = makeTaskCandidate("task1", dims)
	t2 = makeTaskCandidate("task2", dims)
	t3 = makeTaskCandidate("task3", dims)
	rv = getCandidatesToSchedule([]*swarming_api.SwarmingRpcsBotInfo{b1, b2}, []*taskCandidate{t1, t2, t3})
	testutils.AssertDeepEqual(t, []*taskCandidate{t1, t2}, rv)
}

func makeBot(id string, dims map[string]string) *swarming_api.SwarmingRpcsBotInfo {
	dimensions := make([]*swarming_api.SwarmingRpcsStringListPair, 0, len(dims))
	for k, v := range dims {
		dimensions = append(dimensions, &swarming_api.SwarmingRpcsStringListPair{
			Key:   k,
			Value: []string{v},
		})
	}
	return &swarming_api.SwarmingRpcsBotInfo{
		BotId:      id,
		Dimensions: dimensions,
	}
}

func TestSchedulingE2E(t *testing.T) {
	gb, d, swarmingClient, s, _, cleanup := setup(t)
	defer cleanup()

	c1 := getRS1(t, gb).Revision
	c2 := getRS2(t, gb).Revision

	// Start testing. No free bots, so we get a full queue with nothing
	// scheduled.
	assert.NoError(t, s.MainLoop())
	tasks, err := s.tCache.GetTasksForCommits(gb.RepoUrl(), []string{c1, c2})
	assert.NoError(t, err)
	expect := map[string]map[string]*db.Task{
		c1: {},
		c2: {},
	}
	testutils.AssertDeepEqual(t, expect, tasks)
	assert.Equal(t, 2, len(s.queue)) // Two compile tasks.

	// A bot is free but doesn't have all of the right dimensions to run a task.
	bot1 := makeBot("bot1", map[string]string{"pool": "Skia"})
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
	assert.NoError(t, s.MainLoop())
	tasks, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), []string{c1, c2})
	assert.NoError(t, err)
	expect = map[string]map[string]*db.Task{
		c1: {},
		c2: {},
	}
	testutils.AssertDeepEqual(t, expect, tasks)
	assert.Equal(t, 2, len(s.queue)) // Still two compile tasks.

	// One bot free, schedule a task, ensure it's not in the queue.
	bot1.Dimensions = append(bot1.Dimensions, &swarming_api.SwarmingRpcsStringListPair{
		Key:   "os",
		Value: []string{"Ubuntu"},
	})
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), []string{c1, c2})
	assert.NoError(t, err)
	t1 := tasks[c2][specs_testutils.BuildTask]
	assert.NotNil(t, t1)
	assert.Equal(t, c2, t1.Revision)
	assert.Equal(t, specs_testutils.BuildTask, t1.Name)
	assert.Equal(t, []string{c2, c1}, t1.Commits)
	assert.Equal(t, 1, len(s.queue))

	// The task is complete.
	t1.Status = db.TASK_STATUS_SUCCESS
	t1.Finished = time.Now()
	t1.IsolatedOutput = "abc123"
	assert.NoError(t, d.PutTask(t1))
	swarmingClient.MockTasks([]*swarming_api.SwarmingRpcsTaskRequestMetadata{
		makeSwarmingRpcsTaskRequestMetadata(t, t1, linuxTaskDims),
	})

	// No bots free. Ensure that the queue is correct.
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), []string{c1, c2})
	assert.NoError(t, err)
	for _, c := range t1.Commits {
		expect[c][t1.Name] = t1
	}
	testutils.AssertDeepEqual(t, expect, tasks)
	expectLen := 3 // One remaining build task, plus one test task and one perf task.
	assert.Equal(t, expectLen, len(s.queue))

	// More bots than tasks free, ensure the queue is correct.
	bot2 := makeBot("bot2", androidTaskDims)
	bot3 := makeBot("bot3", androidTaskDims)
	bot4 := makeBot("bot4", linuxTaskDims)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2, bot3, bot4})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	_, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), []string{c1, c2})
	assert.NoError(t, err)
	assert.Equal(t, 0, len(s.queue))

	// The build, test, and perf tasks should have triggered.
	var t2 *db.Task
	var t3 *db.Task
	var t4 *db.Task
	tasks, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), []string{c1, c2})
	assert.NoError(t, err)
	assert.Equal(t, 2, len(tasks))
	for commit, v := range tasks {
		if commit == c1 {
			// Build task at c1 and test task at c2 whose blamelist also has c1.
			assert.Equal(t, 2, len(v))
			for _, task := range v {
				if task.Revision != commit {
					continue
				}
				assert.Equal(t, specs_testutils.BuildTask, task.Name)
				assert.Nil(t, t4)
				t4 = task
				assert.Equal(t, c1, task.Revision)
				assert.Equal(t, []string{c1}, task.Commits)
			}
		} else {
			assert.Equal(t, 3, len(v))
			for _, task := range v {
				if task.Name == specs_testutils.TestTask {
					assert.Nil(t, t2)
					t2 = task
					assert.Equal(t, c2, task.Revision)
					assert.Equal(t, []string{c2, c1}, task.Commits)
				} else if task.Name == specs_testutils.PerfTask {
					assert.Nil(t, t3)
					t3 = task
					assert.Equal(t, c2, task.Revision)
					assert.Equal(t, []string{c2}, task.Commits)
				} else {
					// This is the first task we triggered.
					assert.Equal(t, specs_testutils.BuildTask, task.Name)
				}
			}
		}
	}
	assert.NotNil(t, t2)
	assert.NotNil(t, t3)
	assert.NotNil(t, t4)
	t4.Status = db.TASK_STATUS_SUCCESS
	t4.Finished = time.Now()
	t4.IsolatedOutput = "abc123"

	// No new bots free; only the remaining test task should be in the queue.
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{})
	mockTasks := []*swarming_api.SwarmingRpcsTaskRequestMetadata{
		makeSwarmingRpcsTaskRequestMetadata(t, t2, linuxTaskDims),
		makeSwarmingRpcsTaskRequestMetadata(t, t3, linuxTaskDims),
		makeSwarmingRpcsTaskRequestMetadata(t, t4, linuxTaskDims),
	}
	swarmingClient.MockTasks(mockTasks)
	assert.NoError(t, s.updateUnfinishedTasks())
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	expectLen = 1 // Test task from c1
	assert.Equal(t, expectLen, len(s.queue))

	// Finish the other task.
	t3, err = s.tCache.GetTask(t3.Id)
	assert.NoError(t, err)
	t3.Status = db.TASK_STATUS_SUCCESS
	t3.Finished = time.Now()
	t3.IsolatedOutput = "abc123"

	// Ensure that we finalize all of the tasks and insert into the DB.
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2, bot3, bot4})
	mockTasks = []*swarming_api.SwarmingRpcsTaskRequestMetadata{
		makeSwarmingRpcsTaskRequestMetadata(t, t3, linuxTaskDims),
	}
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), []string{c1, c2})
	assert.NoError(t, err)
	assert.Equal(t, 2, len(tasks[c1]))
	assert.Equal(t, 3, len(tasks[c2]))
	assert.Equal(t, 0, len(s.queue))

	// Mark everything as finished. Ensure that the queue still ends up empty.
	tasksList := []*db.Task{}
	for _, v := range tasks {
		for _, task := range v {
			if task.Status != db.TASK_STATUS_SUCCESS {
				task.Status = db.TASK_STATUS_SUCCESS
				task.Finished = time.Now()
				task.IsolatedOutput = "abc123"
				tasksList = append(tasksList, task)
			}
		}
	}
	mockTasks = make([]*swarming_api.SwarmingRpcsTaskRequestMetadata, 0, len(tasksList))
	for _, task := range tasksList {
		mockTasks = append(mockTasks, makeSwarmingRpcsTaskRequestMetadata(t, task, linuxTaskDims))
	}
	swarmingClient.MockTasks(mockTasks)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2, bot3, bot4})
	assert.NoError(t, s.updateUnfinishedTasks())
	assert.NoError(t, s.MainLoop())
	assert.Equal(t, 0, len(s.queue))
}

func makeDummyCommits(gb *git_testutils.GitBuilder, numCommits int) {
	for i := 0; i < numCommits; i++ {
		gb.AddGen("dummyfile.txt")
		gb.CommitMsg(fmt.Sprintf("Dummy #%d/%d", i, numCommits))
	}
}

func TestSchedulerStealingFrom(t *testing.T) {
	gb, d, swarmingClient, s, _, cleanup := setup(t)
	defer cleanup()

	c1 := getRS1(t, gb).Revision
	c2 := getRS2(t, gb).Revision

	// Run the available compile task at c2.
	bot1 := makeBot("bot1", linuxTaskDims)
	bot2 := makeBot("bot2", linuxTaskDims)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err := s.tCache.GetTasksForCommits(gb.RepoUrl(), []string{c1, c2})
	assert.NoError(t, err)
	assert.Equal(t, 1, len(tasks[c1]))
	assert.Equal(t, 1, len(tasks[c2]))

	// Finish the one task.
	tasksList := []*db.Task{}
	t1 := tasks[c2][specs_testutils.BuildTask]
	t1.Status = db.TASK_STATUS_SUCCESS
	t1.Finished = time.Now()
	t1.IsolatedOutput = "abc123"
	tasksList = append(tasksList, t1)

	// Forcibly create and insert a second task at c1.
	t2 := t1.Copy()
	t2.Id = "t2id"
	t2.Revision = c1
	t2.Commits = []string{c1}
	tasksList = append(tasksList, t2)

	assert.NoError(t, d.PutTasks(tasksList))
	assert.NoError(t, s.tCache.Update())

	// Add some commits.
	makeDummyCommits(gb, 10)
	assert.NoError(t, s.repos[gb.RepoUrl()].Repo().Update())
	commits, err := s.repos[gb.RepoUrl()].Repo().RevList("HEAD")
	assert.NoError(t, err)

	// Run one task. Ensure that it's at tip-of-tree.
	head, err := s.repos[gb.RepoUrl()].Repo().RevParse("HEAD")
	assert.NoError(t, err)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), commits)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(tasks[head]))
	task := tasks[head][specs_testutils.BuildTask]
	assert.Equal(t, head, task.Revision)
	expect := commits[:len(commits)-2]
	sort.Strings(expect)
	sort.Strings(task.Commits)
	testutils.AssertDeepEqual(t, expect, task.Commits)

	task.Status = db.TASK_STATUS_SUCCESS
	task.Finished = time.Now()
	task.IsolatedOutput = "abc123"
	assert.NoError(t, d.PutTask(task))
	assert.NoError(t, s.tCache.Update())

	oldTasksByCommit := tasks

	// Run backfills, ensuring that each one steals the right set of commits
	// from previous builds, until all of the build task candidates have run.
	for i := 0; i < 9; i++ {
		// Now, run another task. The new task should bisect the old one.
		swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
		assert.NoError(t, s.MainLoop())
		assert.NoError(t, s.tCache.Update())
		tasks, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), commits)
		assert.NoError(t, err)
		var newTask *db.Task
		for _, v := range tasks {
			for _, task := range v {
				if task.Status == db.TASK_STATUS_PENDING {
					assert.True(t, newTask == nil || task.Id == newTask.Id)
					newTask = task
				}
			}
		}
		assert.NotNil(t, newTask)

		oldTask := oldTasksByCommit[newTask.Revision][newTask.Name]
		assert.NotNil(t, oldTask)
		assert.True(t, util.In(newTask.Revision, oldTask.Commits))

		// Find the updated old task.
		updatedOldTask, err := s.tCache.GetTask(oldTask.Id)
		assert.NoError(t, err)
		assert.NotNil(t, updatedOldTask)

		// Ensure that the blamelists are correct.
		old := util.NewStringSet(oldTask.Commits)
		new := util.NewStringSet(newTask.Commits)
		updatedOld := util.NewStringSet(updatedOldTask.Commits)

		testutils.AssertDeepEqual(t, old, new.Union(updatedOld))
		assert.Equal(t, 0, len(new.Intersect(updatedOld)))
		// Finish the new task.
		newTask.Status = db.TASK_STATUS_SUCCESS
		newTask.Finished = time.Now()
		newTask.IsolatedOutput = "abc123"
		assert.NoError(t, d.PutTask(newTask))
		assert.NoError(t, s.tCache.Update())
		oldTasksByCommit = tasks

	}

	// Ensure that we're really done.
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), commits)
	assert.NoError(t, err)
	var newTask *db.Task
	for _, v := range tasks {
		for _, task := range v {
			if task.Status == db.TASK_STATUS_PENDING {
				assert.True(t, newTask == nil || task.Id == newTask.Id)
				newTask = task
			}
		}
	}
	assert.Nil(t, newTask)
}

// spyDB calls onPutTasks before delegating PutTask(s) to DB.
type spyDB struct {
	db.DB
	onPutTasks func([]*db.Task)
}

func (s *spyDB) PutTask(task *db.Task) error {
	s.onPutTasks([]*db.Task{task})
	return s.DB.PutTask(task)
}

func (s *spyDB) PutTasks(tasks []*db.Task) error {
	s.onPutTasks(tasks)
	return s.DB.PutTasks(tasks)
}

func testMultipleCandidatesBackfillingEachOtherSetup(t *testing.T) (*git_testutils.GitBuilder, db.DB, *TaskScheduler, *swarming.TestClient, []string, func(*db.Task), func()) {
	testutils.LargeTest(t)
	testutils.SkipIfShort(t)

	gb := git_testutils.GitInit(t)
	workdir, err := ioutil.TempDir("", "")
	assert.NoError(t, err)
	assert.NoError(t, os.Mkdir(path.Join(workdir, TRIGGER_DIRNAME), os.ModePerm))

	assert.NoError(t, ioutil.WriteFile(path.Join(workdir, ".gclient"), []byte("dummy"), os.ModePerm))
	infraBotsSubDir := path.Join("infra", "bots")

	gb.Add("somefile.txt", "dummy3")
	gb.Add(path.Join(infraBotsSubDir, "dummy.isolate"), `{
  'variables': {
    'command': [
      'python', 'recipes.py', 'run',
    ],
    'files': [
      '../../somefile.txt',
    ],
  },
}`)

	// Create a single task in the config.
	taskName := "dummytask"
	cfg := &specs.TasksCfg{
		Tasks: map[string]*specs.TaskSpec{
			taskName: {
				CipdPackages: []*specs.CipdPackage{},
				Dependencies: []string{},
				Dimensions:   []string{"pool:Skia"},
				Isolate:      "dummy.isolate",
				Priority:     1.0,
			},
		},
		Jobs: map[string]*specs.JobSpec{
			"j1": {
				TaskSpecs: []string{taskName},
			},
		},
	}
	cfgStr := testutils.MarshalJSON(t, cfg)
	gb.Add(specs.TASKS_CFG_FILE, cfgStr)
	gb.Commit()

	// Setup the scheduler.
	d := db.NewInMemoryDB()
	isolateClient, err := isolate.NewClient(workdir, isolate.ISOLATE_SERVER_URL_FAKE)
	assert.NoError(t, err)
	swarmingClient := swarming.NewTestClient()
	repos, err := repograph.NewMap([]string{gb.RepoUrl()}, workdir)
	assert.NoError(t, err)
	projectRepoMapping := map[string]string{
		"skia": gb.RepoUrl(),
	}
	urlMock := mockhttpclient.NewURLMock()
	depotTools := depot_tools_testutils.GetDepotTools(t)
	gitcookies := path.Join(workdir, "gitcookies_fake")
	assert.NoError(t, ioutil.WriteFile(gitcookies, []byte(".googlesource.com\tTRUE\t/\tTRUE\t123\to\tgit-user.google.com=abc123"), os.ModePerm))
	g, err := gerrit.NewGerrit(fakeGerritUrl, gitcookies, urlMock.Client())
	assert.NoError(t, err)

	s, err := NewTaskScheduler(d, time.Duration(math.MaxInt64), 0, workdir, "fake.server", repos, isolateClient, swarmingClient, mockhttpclient.NewURLMock().Client(), 1.0, tryjobs.API_URL_TESTING, tryjobs.BUCKET_TESTING, projectRepoMapping, swarming.POOLS_PUBLIC, "", depotTools, g)
	assert.NoError(t, err)

	mockTasks := []*swarming_api.SwarmingRpcsTaskRequestMetadata{}
	mock := func(task *db.Task) {
		task.Status = db.TASK_STATUS_SUCCESS
		task.Finished = time.Now()
		task.IsolatedOutput = "abc123"
		mockTasks = append(mockTasks, makeSwarmingRpcsTaskRequestMetadata(t, task, linuxTaskDims))
		swarmingClient.MockTasks(mockTasks)
	}

	// Cycle once.
	bot1 := makeBot("bot1", map[string]string{"pool": "Skia"})
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	assert.Equal(t, 0, len(s.queue))
	head, err := s.repos[gb.RepoUrl()].Repo().RevParse("HEAD")
	assert.NoError(t, err)
	tasks, err := s.tCache.GetTasksForCommits(gb.RepoUrl(), []string{head})
	assert.NoError(t, err)
	assert.Equal(t, 1, len(tasks[head]))
	mock(tasks[head][taskName])

	// Add some commits to the repo.
	gb.CheckoutBranch("master")
	makeDummyCommits(gb, 8)
	assert.NoError(t, s.repos[gb.RepoUrl()].Repo().Update())
	commits, err := s.repos[gb.RepoUrl()].Repo().RevList(fmt.Sprintf("%s..HEAD", head))
	assert.Nil(t, err)
	assert.Equal(t, 8, len(commits))
	return gb, d, s, swarmingClient, commits, mock, func() {
		gb.Cleanup()
		testutils.RemoveAll(t, workdir)
	}
}

func TestMultipleCandidatesBackfillingEachOther(t *testing.T) {
	gb, d, s, swarmingClient, commits, mock, cleanup := testMultipleCandidatesBackfillingEachOtherSetup(t)
	defer cleanup()

	// Trigger builds simultaneously.
	bot1 := makeBot("bot1", map[string]string{"pool": "Skia"})
	bot2 := makeBot("bot2", map[string]string{"pool": "Skia"})
	bot3 := makeBot("bot3", map[string]string{"pool": "Skia"})
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2, bot3})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	assert.Equal(t, 5, len(s.queue))
	tasks, err := s.tCache.GetTasksForCommits(gb.RepoUrl(), commits)
	assert.NoError(t, err)

	// If we're queueing correctly, we should've triggered tasks at
	// commits[0], commits[4], and either commits[2] or commits[6].
	var t1, t2, t3 *db.Task
	for _, byName := range tasks {
		for _, task := range byName {
			if task.Revision == commits[0] {
				t1 = task
			} else if task.Revision == commits[4] {
				t2 = task
			} else if task.Revision == commits[2] || task.Revision == commits[6] {
				t3 = task
			} else {
				assert.FailNow(t, fmt.Sprintf("Task has unknown revision: %v", task))
			}
		}
	}
	assert.NotNil(t, t1)
	assert.NotNil(t, t2)
	assert.NotNil(t, t3)
	mock(t1)
	mock(t2)
	mock(t3)

	// Ensure that we got the blamelists right.
	var expect1, expect2, expect3 []string
	if t3.Revision == commits[2] {
		expect1 = util.CopyStringSlice(commits[:2])
		expect2 = util.CopyStringSlice(commits[4:])
		expect3 = util.CopyStringSlice(commits[2:4])
	} else {
		expect1 = util.CopyStringSlice(commits[:4])
		expect2 = util.CopyStringSlice(commits[4:6])
		expect3 = util.CopyStringSlice(commits[6:])
	}
	sort.Strings(expect1)
	sort.Strings(expect2)
	sort.Strings(expect3)
	sort.Strings(t1.Commits)
	sort.Strings(t2.Commits)
	sort.Strings(t3.Commits)
	testutils.AssertDeepEqual(t, expect1, t1.Commits)
	testutils.AssertDeepEqual(t, expect2, t2.Commits)
	testutils.AssertDeepEqual(t, expect3, t3.Commits)

	// Just for good measure, check the task at the head of the queue.
	expectIdx := 2
	if t3.Revision == commits[expectIdx] {
		expectIdx = 6
	}
	assert.Equal(t, commits[expectIdx], s.queue[0].Revision)

	retryCount := 0
	causeConcurrentUpdate := func(tasks []*db.Task) {
		// HACK(benjaminwagner): Filter out PutTask calls from
		// updateUnfinishedTasks by looking for new tasks.
		anyNew := false
		for _, task := range tasks {
			if util.TimeIsZero(task.DbModified) {
				anyNew = true
				break
			}
		}
		if !anyNew {
			return
		}
		if retryCount < 3 {
			taskToUpdate := []*db.Task{t1, t2, t3}[retryCount]
			retryCount++
			taskInDb, err := d.GetTaskById(taskToUpdate.Id)
			assert.NoError(t, err)
			taskInDb.Status = db.TASK_STATUS_SUCCESS
			assert.NoError(t, d.PutTask(taskInDb))
		}
	}
	s.db = &spyDB{
		DB:         d,
		onPutTasks: causeConcurrentUpdate,
	}

	// Run again with 5 bots to check the case where we bisect the same
	// task twice.
	bot4 := makeBot("bot4", map[string]string{"pool": "Skia"})
	bot5 := makeBot("bot5", map[string]string{"pool": "Skia"})
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2, bot3, bot4, bot5})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	assert.Equal(t, 0, len(s.queue))
	assert.Equal(t, 3, retryCount)
	tasks, err = s.tCache.GetTasksForCommits(gb.RepoUrl(), commits)
	assert.NoError(t, err)
	for _, byName := range tasks {
		for _, task := range byName {
			assert.Equal(t, 1, len(task.Commits))
			assert.Equal(t, task.Revision, task.Commits[0])
			if util.In(task.Id, []string{t1.Id, t2.Id, t3.Id}) {
				assert.Equal(t, db.TASK_STATUS_SUCCESS, task.Status)
			} else {
				assert.Equal(t, db.TASK_STATUS_PENDING, task.Status)
			}
		}
	}
}

func TestSchedulingRetry(t *testing.T) {
	gb, d, swarmingClient, s, _, cleanup := setup(t)
	defer cleanup()

	c1 := getRS1(t, gb).Revision

	// Run the available compile task at c2.
	bot1 := makeBot("bot1", linuxTaskDims)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err := s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(tasks))
	t1 := tasks[0]
	assert.NotNil(t, t1)
	// Ensure c2, not c1.
	assert.NotEqual(t, c1, t1.Revision)
	c2 := t1.Revision

	// Forcibly add a second build task at c1.
	t2 := t1.Copy()
	t2.Id = "t2Id"
	t2.Revision = c1
	t2.Commits = []string{c1}
	t1.Commits = []string{c2}

	// One task successful, the other not.
	t1.Status = db.TASK_STATUS_FAILURE
	t1.Finished = time.Now()
	t2.Status = db.TASK_STATUS_SUCCESS
	t2.Finished = time.Now()
	t2.IsolatedOutput = "abc123"

	assert.NoError(t, d.PutTasks([]*db.Task{t1, t2}))
	assert.NoError(t, s.tCache.Update())

	// Cycle. Ensure that we schedule a retry of t1.
	prev := t1
	i := 1
	for {
		assert.NoError(t, s.MainLoop())
		assert.NoError(t, s.tCache.Update())
		tasks, err = s.tCache.UnfinishedTasks()
		assert.NoError(t, err)
		if len(tasks) == 0 {
			break
		}
		assert.Equal(t, 1, len(tasks))
		retry := tasks[0]
		assert.NotNil(t, retry)
		assert.Equal(t, prev.Id, retry.RetryOf)
		assert.Equal(t, i, retry.Attempt)
		assert.Equal(t, c2, retry.Revision)
		retry.Status = db.TASK_STATUS_FAILURE
		retry.Finished = time.Now()
		assert.NoError(t, d.PutTask(retry))
		assert.NoError(t, s.tCache.Update())

		prev = retry
		i++
	}
	assert.Equal(t, 5, i)
}

func TestParentTaskId(t *testing.T) {
	_, d, swarmingClient, s, _, cleanup := setup(t)
	defer cleanup()

	// Run the available compile task at c2.
	bot1 := makeBot("bot1", linuxTaskDims)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err := s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(tasks))
	t1 := tasks[0]
	t1.Status = db.TASK_STATUS_SUCCESS
	t1.Finished = time.Now()
	t1.IsolatedOutput = "abc123"
	assert.Equal(t, 0, len(t1.ParentTaskIds))
	assert.NoError(t, d.PutTasks([]*db.Task{t1}))
	assert.NoError(t, s.tCache.Update())

	// Run the dependent tasks. Ensure that their parent IDs are correct.
	bot3 := makeBot("bot3", androidTaskDims)
	bot4 := makeBot("bot4", androidTaskDims)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot3, bot4})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err = s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	assert.Equal(t, 2, len(tasks))
	for _, task := range tasks {
		assert.Equal(t, 1, len(task.ParentTaskIds))
		p := task.ParentTaskIds[0]
		assert.Equal(t, p, t1.Id)

		updated, err := task.UpdateFromSwarming(makeSwarmingRpcsTaskRequestMetadata(t, task, linuxTaskDims).TaskResult)
		assert.NoError(t, err)
		assert.False(t, updated)
	}
}

func TestBlacklist(t *testing.T) {
	// The blacklist has its own tests, so this test just verifies that it's
	// actually integrated into the scheduler.
	gb, _, swarmingClient, s, _, cleanup := setup(t)
	defer cleanup()

	c1 := getRS1(t, gb).Revision

	// Mock some bots, add one of the build tasks to the blacklist.
	bot1 := makeBot("bot1", linuxTaskDims)
	bot2 := makeBot("bot2", linuxTaskDims)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2})
	assert.NoError(t, s.GetBlacklist().AddRule(&blacklist.Rule{
		AddedBy:          "Tests",
		TaskSpecPatterns: []string{".*"},
		Commits:          []string{c1},
		Description:      "desc",
		Name:             "My-Rule",
	}, s.repos))
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err := s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	// The blacklisted commit should not have been triggered.
	assert.Equal(t, 1, len(tasks))
	assert.NotEqual(t, c1, tasks[0].Revision)
}

func TestTrybots(t *testing.T) {
	gb, d, swarmingClient, s, mock, cleanup := setup(t)
	defer cleanup()

	rs2 := getRS2(t, gb)

	// The trybot integrator has its own tests, so just verify that we can
	// receive a try request, execute the necessary tasks, and report its
	// results back.

	// Run ourselves out of tasks.
	bot1 := makeBot("bot1", linuxTaskDims)
	bot2 := makeBot("bot2", androidTaskDims)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2})
	now := time.Now()

	n := 0
	for i := 0; i < 10; i++ {
		assert.NoError(t, s.MainLoop())
		assert.NoError(t, s.tCache.Update())
		tasks, err := s.tCache.UnfinishedTasks()
		assert.NoError(t, err)
		if len(tasks) == 0 {
			break
		}
		for _, t := range tasks {
			t.Status = db.TASK_STATUS_SUCCESS
			t.Finished = now
			t.IsolatedOutput = "abc123"
			n++
		}
		assert.NoError(t, d.PutTasks(tasks))
		assert.NoError(t, s.tCache.Update())
	}
	assert.Equal(t, 5, n)
	jobs, err := s.jCache.UnfinishedJobs()
	assert.NoError(t, err)
	assert.Equal(t, 0, len(jobs))

	// Create a try job.
	issue := "10001"
	patchset := "20002"
	gb.CreateFakeGerritCLGen(issue, patchset)

	b := tryjobs.Build(t, now)
	rs := db.RepoState{
		Patch: db.Patch{
			Server:    gb.RepoUrl(),
			Issue:     issue,
			PatchRepo: rs2.Repo,
			Patchset:  patchset,
		},
		Repo:     rs2.Repo,
		Revision: rs2.Revision,
	}
	b.ParametersJson = testutils.MarshalJSON(t, tryjobs.Params(t, specs_testutils.TestTask, "skia", rs.Revision, rs.Server, rs.Issue, rs.Patchset))
	tryjobs.MockPeek(mock, []*buildbucket_api.ApiBuildMessage{b}, now, "", "", nil)
	tryjobs.MockTryLeaseBuild(mock, b.Id, now, nil)
	tryjobs.MockJobStarted(mock, b.Id, now, nil)
	assert.NoError(t, s.tryjobs.Poll(now))
	assert.True(t, mock.Empty())

	// Ensure that we added a Job.
	assert.NoError(t, s.jCache.Update())
	jobs, err = s.jCache.UnfinishedJobs()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(jobs))
	var tryJob *db.Job
	for _, j := range jobs {
		if j.IsTryJob() {
			tryJob = j
			break
		}
	}
	assert.NotNil(t, tryJob)
	assert.False(t, tryJob.Done())

	// Run through the try job's tasks.
	for i := 0; i < 10; i++ {
		assert.NoError(t, s.MainLoop())
		assert.NoError(t, s.tCache.Update())
		tasks, err := s.tCache.UnfinishedTasks()
		assert.NoError(t, err)
		if len(tasks) == 0 {
			break
		}
		for _, task := range tasks {
			assert.Equal(t, rs, task.RepoState)
			assert.Equal(t, 0, len(task.Commits))
			task.Status = db.TASK_STATUS_SUCCESS
			task.Finished = now
			task.IsolatedOutput = "abc123"
			n++
		}
		assert.NoError(t, d.PutTasks(tasks))
		assert.NoError(t, s.tCache.Update())
	}
	assert.True(t, mock.Empty())

	// Some final checks.
	assert.NoError(t, s.jCache.Update())
	assert.Equal(t, 7, n)
	tryJob, err = s.jCache.GetJob(tryJob.Id)
	assert.NoError(t, err)
	assert.True(t, tryJob.IsTryJob())
	assert.True(t, tryJob.Done())
	assert.True(t, tryJob.Finished.After(tryJob.Created))
	jobs, err = s.jCache.UnfinishedJobs()
	assert.NoError(t, err)
	assert.Equal(t, 0, len(jobs))
}

func TestGetTasksForJob(t *testing.T) {
	gb, d, swarmingClient, s, _, cleanup := setup(t)
	defer cleanup()

	c1 := getRS1(t, gb).Revision
	c2 := getRS2(t, gb).Revision

	// Cycle once, check that we have empty sets for all Jobs.
	assert.NoError(t, s.MainLoop())
	jobs, err := s.jCache.UnfinishedJobs()
	assert.NoError(t, err)
	assert.Equal(t, 5, len(jobs))
	var j1, j2, j3, j4, j5 *db.Job
	for _, j := range jobs {
		if j.Revision == c1 {
			if j.Name == specs_testutils.BuildTask {
				j1 = j
			} else {
				j2 = j
			}
		} else {
			if j.Name == specs_testutils.BuildTask {
				j3 = j
			} else if j.Name == specs_testutils.TestTask {
				j4 = j
			} else {
				j5 = j
			}
		}
		tasksByName, err := s.getTasksForJob(j)
		assert.NoError(t, err)
		for _, tasks := range tasksByName {
			assert.Equal(t, 0, len(tasks))
		}
	}
	assert.NotNil(t, j1)
	assert.NotNil(t, j2)
	assert.NotNil(t, j3)
	assert.NotNil(t, j4)
	assert.NotNil(t, j5)

	// Run the available compile task at c2.
	bot1 := makeBot("bot1", linuxTaskDims)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err := s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(tasks))
	t1 := tasks[0]
	assert.NotNil(t, t1)
	assert.Equal(t, t1.Revision, c2)

	// Test that we get the new tasks where applicable.
	expect := map[string]map[string][]*db.Task{
		j1.Id: {
			specs_testutils.BuildTask: {},
		},
		j2.Id: {
			specs_testutils.BuildTask: {},
			specs_testutils.TestTask:  {},
		},
		j3.Id: {
			specs_testutils.BuildTask: {t1},
		},
		j4.Id: {
			specs_testutils.BuildTask: {t1},
			specs_testutils.TestTask:  {},
		},
		j5.Id: {
			specs_testutils.BuildTask: {t1},
			specs_testutils.PerfTask:  {},
		},
	}
	for _, j := range jobs {
		tasksByName, err := s.getTasksForJob(j)
		assert.NoError(t, err)
		testutils.AssertDeepEqual(t, expect[j.Id], tasksByName)
	}

	// Mark the task successful.
	t1.Status = db.TASK_STATUS_FAILURE
	t1.Finished = time.Now()
	assert.NoError(t, d.PutTasks([]*db.Task{t1}))
	assert.NoError(t, s.tCache.Update())

	// Test that the results propagated through.
	for _, j := range jobs {
		tasksByName, err := s.getTasksForJob(j)
		assert.NoError(t, err)
		testutils.AssertDeepEqual(t, expect[j.Id], tasksByName)
	}

	// Cycle. Ensure that we schedule a retry of t1.
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err = s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(tasks))
	t2 := tasks[0]
	assert.NotNil(t, t2)
	assert.Equal(t, t1.Id, t2.RetryOf)

	// Verify that both the original t1 and its retry show up.
	t1, err = s.tCache.GetTask(t1.Id) // t1 was updated.
	assert.NoError(t, err)
	expect[j3.Id][specs_testutils.BuildTask] = []*db.Task{t1, t2}
	expect[j4.Id][specs_testutils.BuildTask] = []*db.Task{t1, t2}
	expect[j5.Id][specs_testutils.BuildTask] = []*db.Task{t1, t2}
	for _, j := range jobs {
		tasksByName, err := s.getTasksForJob(j)
		assert.NoError(t, err)
		testutils.AssertDeepEqual(t, expect[j.Id], tasksByName)
	}

	// The retry succeeded.
	t2.Status = db.TASK_STATUS_SUCCESS
	t2.Finished = time.Now()
	t2.IsolatedOutput = "abc"
	assert.NoError(t, d.PutTask(t2))
	assert.NoError(t, s.tCache.Update())
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	tasks, err = s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	assert.Equal(t, 0, len(tasks))

	// Test that the results propagated through.
	for _, j := range jobs {
		tasksByName, err := s.getTasksForJob(j)
		assert.NoError(t, err)
		testutils.AssertDeepEqual(t, expect[j.Id], tasksByName)
	}

	// Schedule the remaining tasks.
	bot3 := makeBot("bot3", androidTaskDims)
	bot4 := makeBot("bot4", androidTaskDims)
	bot5 := makeBot("bot5", androidTaskDims)
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot3, bot4, bot5})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())

	// Verify that the new tasks show up.
	tasks, err = s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	assert.Equal(t, 2, len(tasks)) // Test and perf at c2.
	var t4, t5 *db.Task
	for _, task := range tasks {
		if task.Name == specs_testutils.TestTask {
			t4 = task
		} else {
			t5 = task
		}
	}
	expect[j4.Id][specs_testutils.TestTask] = []*db.Task{t4}
	expect[j5.Id][specs_testutils.PerfTask] = []*db.Task{t5}
	for _, j := range jobs {
		tasksByName, err := s.getTasksForJob(j)
		assert.NoError(t, err)
		testutils.AssertDeepEqual(t, expect[j.Id], tasksByName)
	}
}

func TestTaskTimeouts(t *testing.T) {
	gb, _, swarmingClient, s, _, cleanup := setup(t)
	defer cleanup()

	// The test repo does not set any timeouts. Ensure that we get
	// reasonable default values.
	bot1 := makeBot("bot1", map[string]string{"pool": "Skia", "os": "Ubuntu", "gpu": "none"})
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	unfinished, err := s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(unfinished))
	task := unfinished[0]
	swarmingTask, err := swarmingClient.GetTaskMetadata(task.SwarmingTaskId)
	assert.NoError(t, err)
	// These are the defaults in go/swarming/swarming.go.
	assert.Equal(t, int64(60*60), swarmingTask.Request.Properties.ExecutionTimeoutSecs)
	assert.Equal(t, int64(20*60), swarmingTask.Request.Properties.IoTimeoutSecs)
	assert.Equal(t, int64(4*60*60), swarmingTask.Request.ExpirationSecs)
	// Fail the task to get it out of the unfinished list.
	task.Status = db.TASK_STATUS_FAILURE
	assert.NoError(t, s.db.PutTask(task))

	// Rewrite tasks.json with some timeouts.
	name := "Timeout-Task"
	cfg := &specs.TasksCfg{
		Jobs: map[string]*specs.JobSpec{
			"Timeout-Job": {
				Priority:  1.0,
				TaskSpecs: []string{name},
			},
		},
		Tasks: map[string]*specs.TaskSpec{
			name: {
				CipdPackages: []*specs.CipdPackage{},
				Dependencies: []string{},
				Dimensions: []string{
					"pool:Skia",
					"os:Mac",
					"gpu:my-gpu",
				},
				ExecutionTimeout: 40 * time.Minute,
				Expiration:       2 * time.Hour,
				IoTimeout:        3 * time.Minute,
				Isolate:          "compile_skia.isolate",
				Priority:         1.0,
			},
		},
	}
	gb.Add(specs.TASKS_CFG_FILE, testutils.MarshalJSON(t, &cfg))
	gb.Commit()

	// Cycle, ensure that we get the expected timeouts.
	bot2 := makeBot("bot2", map[string]string{"pool": "Skia", "os": "Mac", "gpu": "my-gpu"})
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot2})
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.tCache.Update())
	unfinished, err = s.tCache.UnfinishedTasks()
	assert.NoError(t, err)
	assert.Equal(t, 1, len(unfinished))
	task = unfinished[0]
	assert.Equal(t, name, task.Name)
	swarmingTask, err = swarmingClient.GetTaskMetadata(task.SwarmingTaskId)
	assert.NoError(t, err)
	assert.Equal(t, int64(40*60), swarmingTask.Request.Properties.ExecutionTimeoutSecs)
	assert.Equal(t, int64(3*60), swarmingTask.Request.Properties.IoTimeoutSecs)
	assert.Equal(t, int64(2*60*60), swarmingTask.Request.ExpirationSecs)
}

func TestPeriodicJobs(t *testing.T) {
	gb, _, _, s, _, cleanup := setup(t)
	defer cleanup()

	// Rewrite tasks.json with a periodic job.
	name := "Periodic-Task"
	cfg := &specs.TasksCfg{
		Jobs: map[string]*specs.JobSpec{
			"Periodic-Job": {
				Priority:  1.0,
				TaskSpecs: []string{name},
				Trigger:   "nightly",
			},
		},
		Tasks: map[string]*specs.TaskSpec{
			name: {
				CipdPackages: []*specs.CipdPackage{},
				Dependencies: []string{},
				Dimensions: []string{
					"pool:Skia",
					"os:Mac",
					"gpu:my-gpu",
				},
				ExecutionTimeout: 40 * time.Minute,
				Expiration:       2 * time.Hour,
				IoTimeout:        3 * time.Minute,
				Isolate:          "compile_skia.isolate",
				Priority:         1.0,
			},
		},
	}
	gb.Add(specs.TASKS_CFG_FILE, testutils.MarshalJSON(t, &cfg))
	gb.Commit()

	// Cycle, ensure that the periodic task is not added.
	assert.NoError(t, s.MainLoop())
	assert.NoError(t, s.jCache.Update())
	unfinished, err := s.jCache.UnfinishedJobs()
	assert.NoError(t, err)
	assert.Equal(t, 5, len(unfinished)) // Existing per-commit jobs.
	assert.Equal(t, 0, len(s.triggerMetrics.LastTriggered))
	assert.Equal(t, 0, len(s.triggerMetrics.metrics))

	// Write the trigger file. Cycle, ensure that the trigger file was
	// removed and the periodic task was added.
	triggerFile := path.Join(s.workdir, TRIGGER_DIRNAME, "nightly")
	assert.NoError(t, ioutil.WriteFile(triggerFile, []byte{}, os.ModePerm))
	assert.NoError(t, s.MainLoop())
	_, err = os.Stat(triggerFile)
	assert.True(t, os.IsNotExist(err))
	assert.NoError(t, s.jCache.Update())
	unfinished, err = s.jCache.UnfinishedJobs()
	assert.NoError(t, err)
	assert.Equal(t, 6, len(unfinished))
	assert.Equal(t, 1, len(s.triggerMetrics.LastTriggered))
	assert.Equal(t, 1, len(s.triggerMetrics.metrics))

	// Test the metrics by creating a new periodicTriggerMetrics and
	// verifying that we get the same last-triggered time.
	metrics, err := newPeriodicTriggerMetrics(s.workdir)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(metrics.LastTriggered))
	assert.Equal(t, 1, len(metrics.metrics))
	assert.Equal(t, s.triggerMetrics.LastTriggered["nightly"].Unix(), metrics.LastTriggered["nightly"].Unix())
}

func TestUpdateUnfinishedTasks(t *testing.T) {
	_, _, swarmingClient, s, _, cleanup := setup(t)
	defer cleanup()

	// Create a few tasks.
	now := time.Unix(1480683321, 0).UTC()
	t1 := &db.Task{
		Id:             "t1",
		Created:        now.Add(-time.Minute),
		Status:         db.TASK_STATUS_RUNNING,
		SwarmingTaskId: "swarmt1",
	}
	t2 := &db.Task{
		Id:             "t2",
		Created:        now.Add(-10 * time.Minute),
		Status:         db.TASK_STATUS_PENDING,
		SwarmingTaskId: "swarmt2",
	}
	t3 := &db.Task{
		Id:             "t3",
		Created:        now.Add(-5 * time.Hour), // Outside the 4-hour window.
		Status:         db.TASK_STATUS_PENDING,
		SwarmingTaskId: "swarmt3",
	}
	// Include a fake task to ensure it's ignored.
	t4 := &db.Task{
		Id:      "t4",
		Created: now.Add(-time.Minute),
		Status:  db.TASK_STATUS_PENDING,
	}

	// Insert the tasks into the DB.
	tasks := []*db.Task{t1, t2, t3, t4}
	assert.NoError(t, s.db.PutTasks(tasks))
	assert.NoError(t, s.tCache.Update())

	// Update the tasks, mock in Swarming.
	t1.Status = db.TASK_STATUS_SUCCESS
	t2.Status = db.TASK_STATUS_FAILURE
	t3.Status = db.TASK_STATUS_SUCCESS

	m1 := makeSwarmingRpcsTaskRequestMetadata(t, t1, linuxTaskDims)
	m2 := makeSwarmingRpcsTaskRequestMetadata(t, t2, linuxTaskDims)
	m3 := makeSwarmingRpcsTaskRequestMetadata(t, t3, linuxTaskDims)
	swarmingClient.MockTasks([]*swarming_api.SwarmingRpcsTaskRequestMetadata{m1, m2, m3})

	// Assert that the third task doesn't show up in the time range query.
	got, err := swarmingClient.ListTasks(now.Add(-4*time.Hour), now, []string{"pool:Skia"}, "")
	assert.NoError(t, err)
	testutils.AssertDeepEqual(t, []*swarming_api.SwarmingRpcsTaskRequestMetadata{m1, m2}, got)

	// Ensure that we update the tasks as expected.
	assert.NoError(t, s.updateUnfinishedTasks())
	for _, task := range tasks {
		got, err := s.db.GetTaskById(task.Id)
		assert.NoError(t, err)
		// Ignore DbModified when comparing.
		task.DbModified = got.DbModified
		testutils.AssertDeepEqual(t, task, got)
	}
}

// setupAddTasksTest calls setup then adds 7 commits to the repo and returns
// their hashes.
func setupAddTasksTest(t *testing.T) (*git_testutils.GitBuilder, []string, db.DB, *TaskScheduler, func()) {
	gb, d, _, s, _, cleanup := setup(t)

	// Add some commits to test blamelist calculation.
	makeDummyCommits(gb, 7)
	assert.NoError(t, s.updateRepos())
	hashes, err := s.repos[gb.RepoUrl()].Repo().RevList("HEAD")
	assert.NoError(t, err)

	return gb, hashes, d, s, func() {
		cleanup()
	}
}

// assertBlamelist asserts task.Commits contains exactly the hashes at the given
// indexes of hashes, in any order.
func assertBlamelist(t *testing.T, hashes []string, task *db.Task, indexes []int) {
	expected := util.NewStringSet()
	for _, idx := range indexes {
		expected[hashes[idx]] = true
	}
	assert.Equal(t, expected, util.NewStringSet(task.Commits))
}

// assertModifiedTasks asserts that the result of GetModifiedTasks is deep-equal
// to expected, in any order.
func assertModifiedTasks(t *testing.T, d db.TaskReader, id string, expected []*db.Task) {
	tasksById := map[string]*db.Task{}
	modTasks, err := d.GetModifiedTasks(id)
	assert.NoError(t, err)
	assert.Equal(t, len(expected), len(modTasks))
	for _, task := range modTasks {
		tasksById[task.Id] = task
	}

	assert.Equal(t, len(expected), len(tasksById))
	for i, expectedTask := range expected {
		actualTask, ok := tasksById[expectedTask.Id]
		assert.True(t, ok, "Missing task; idx %d; id %s", i, expectedTask.Id)
		testutils.AssertDeepEqual(t, expectedTask, actualTask)
	}
}

// addTasksSingleTaskSpec should add tasks and compute simple blamelists.
func TestAddTasksSingleTaskSpecSimple(t *testing.T) {
	gb, hashes, d, s, cleanup := setupAddTasksTest(t)
	defer cleanup()

	t1 := makeTask("toil", gb.RepoUrl(), hashes[6])
	assert.NoError(t, d.PutTask(t1))
	assert.NoError(t, s.tCache.Update())

	trackId, err := d.StartTrackingModifiedTasks()
	assert.NoError(t, err)

	t2 := makeTask("toil", gb.RepoUrl(), hashes[5]) // Commits should be {5}
	t3 := makeTask("toil", gb.RepoUrl(), hashes[3]) // Commits should be {3, 4}
	t4 := makeTask("toil", gb.RepoUrl(), hashes[2]) // Commits should be {2}
	t5 := makeTask("toil", gb.RepoUrl(), hashes[0]) // Commits should be {0, 1}

	// Clear Commits on some tasks, set incorrect Commits on others to
	// ensure it's ignored.
	t3.Commits = nil
	t4.Commits = []string{hashes[5], hashes[4], hashes[3], hashes[2]}
	sort.Strings(t4.Commits)

	// Specify tasks in wrong order to ensure results are deterministic.
	assert.NoError(t, s.addTasksSingleTaskSpec([]*db.Task{t5, t2, t3, t4}))

	assertBlamelist(t, hashes, t2, []int{5})
	assertBlamelist(t, hashes, t3, []int{3, 4})
	assertBlamelist(t, hashes, t4, []int{2})
	assertBlamelist(t, hashes, t5, []int{0, 1})

	// Check that the tasks were inserted into the DB.
	assertModifiedTasks(t, d, trackId, []*db.Task{t2, t3, t4, t5})
}

// addTasksSingleTaskSpec should compute blamelists when new tasks bisect each
// other.
func TestAddTasksSingleTaskSpecBisectNew(t *testing.T) {
	gb, hashes, d, s, cleanup := setupAddTasksTest(t)
	defer cleanup()

	t1 := makeTask("toil", gb.RepoUrl(), hashes[6])
	assert.NoError(t, d.PutTask(t1))
	assert.NoError(t, s.tCache.Update())

	trackId, err := d.StartTrackingModifiedTasks()
	assert.NoError(t, err)

	// t2.Commits = {1, 2, 3, 4, 5}
	t2 := makeTask("toil", gb.RepoUrl(), hashes[1])
	// t3.Commits = {3, 4, 5}
	// t2.Commits = {1, 2}
	t3 := makeTask("toil", gb.RepoUrl(), hashes[3])
	// t4.Commits = {4, 5}
	// t3.Commits = {3}
	t4 := makeTask("toil", gb.RepoUrl(), hashes[4])
	// t5.Commits = {0}
	t5 := makeTask("toil", gb.RepoUrl(), hashes[0])
	// t6.Commits = {2}
	// t2.Commits = {1}
	t6 := makeTask("toil", gb.RepoUrl(), hashes[2])
	// t7.Commits = {1}
	// t2.Commits = {}
	t7 := makeTask("toil", gb.RepoUrl(), hashes[1])

	// Specify tasks in wrong order to ensure results are deterministic.
	tasks := []*db.Task{t5, t2, t7, t3, t6, t4}

	// Assign Ids.
	for _, task := range tasks {
		assert.NoError(t, d.AssignId(task))
	}

	assert.NoError(t, s.addTasksSingleTaskSpec(tasks))

	assertBlamelist(t, hashes, t2, []int{})
	assertBlamelist(t, hashes, t3, []int{3})
	assertBlamelist(t, hashes, t4, []int{4, 5})
	assertBlamelist(t, hashes, t5, []int{0})
	assertBlamelist(t, hashes, t6, []int{2})
	assertBlamelist(t, hashes, t7, []int{1})

	// Check that the tasks were inserted into the DB.
	assertModifiedTasks(t, d, trackId, tasks)
}

// addTasksSingleTaskSpec should compute blamelists when new tasks bisect old
// tasks.
func TestAddTasksSingleTaskSpecBisectOld(t *testing.T) {
	gb, hashes, d, s, cleanup := setupAddTasksTest(t)
	defer cleanup()

	t1 := makeTask("toil", gb.RepoUrl(), hashes[6])
	t2 := makeTask("toil", gb.RepoUrl(), hashes[1])
	t2.Commits = []string{hashes[1], hashes[2], hashes[3], hashes[4], hashes[5]}
	sort.Strings(t2.Commits)
	assert.NoError(t, d.PutTasks([]*db.Task{t1, t2}))
	assert.NoError(t, s.tCache.Update())

	trackId, err := d.StartTrackingModifiedTasks()
	assert.NoError(t, err)

	// t3.Commits = {3, 4, 5}
	// t2.Commits = {1, 2}
	t3 := makeTask("toil", gb.RepoUrl(), hashes[3])
	// t4.Commits = {4, 5}
	// t3.Commits = {3}
	t4 := makeTask("toil", gb.RepoUrl(), hashes[4])
	// t5.Commits = {2}
	// t2.Commits = {1}
	t5 := makeTask("toil", gb.RepoUrl(), hashes[2])
	// t6.Commits = {4, 5}
	// t4.Commits = {}
	t6 := makeTask("toil", gb.RepoUrl(), hashes[4])

	// Specify tasks in wrong order to ensure results are deterministic.
	assert.NoError(t, s.addTasksSingleTaskSpec([]*db.Task{t5, t3, t6, t4}))

	t2Updated, err := d.GetTaskById(t2.Id)
	assert.NoError(t, err)
	assertBlamelist(t, hashes, t2Updated, []int{1})
	assertBlamelist(t, hashes, t3, []int{3})
	assertBlamelist(t, hashes, t4, []int{})
	assertBlamelist(t, hashes, t5, []int{2})
	assertBlamelist(t, hashes, t6, []int{4, 5})

	// Check that the tasks were inserted into the DB.
	t2.Commits = t2Updated.Commits
	t2.DbModified = t2Updated.DbModified
	assertModifiedTasks(t, d, trackId, []*db.Task{t2, t3, t4, t5, t6})
}

// addTasksSingleTaskSpec should update existing tasks, keeping the correct
// blamelist.
func TestAddTasksSingleTaskSpecUpdate(t *testing.T) {
	gb, hashes, d, s, cleanup := setupAddTasksTest(t)
	defer cleanup()

	t1 := makeTask("toil", gb.RepoUrl(), hashes[6])
	t2 := makeTask("toil", gb.RepoUrl(), hashes[3])
	t3 := makeTask("toil", gb.RepoUrl(), hashes[4])
	t3.Commits = nil // Stolen by t5
	t4 := makeTask("toil", gb.RepoUrl(), hashes[0])
	t4.Commits = []string{hashes[0], hashes[1], hashes[2]}
	sort.Strings(t4.Commits)
	t5 := makeTask("toil", gb.RepoUrl(), hashes[4])
	t5.Commits = []string{hashes[4], hashes[5]}
	sort.Strings(t5.Commits)

	tasks := []*db.Task{t1, t2, t3, t4, t5}
	assert.NoError(t, d.PutTasks(tasks))
	assert.NoError(t, s.tCache.Update())

	trackId, err := d.StartTrackingModifiedTasks()
	assert.NoError(t, err)

	// Make an update.
	for _, task := range tasks {
		task.Status = db.TASK_STATUS_MISHAP
	}

	// Specify tasks in wrong order to ensure results are deterministic.
	assert.NoError(t, s.addTasksSingleTaskSpec([]*db.Task{t5, t3, t1, t4, t2}))

	// Check that blamelists did not change.
	assertBlamelist(t, hashes, t1, []int{6})
	assertBlamelist(t, hashes, t2, []int{3})
	assertBlamelist(t, hashes, t3, []int{})
	assertBlamelist(t, hashes, t4, []int{0, 1, 2})
	assertBlamelist(t, hashes, t5, []int{4, 5})

	// Check that the tasks were inserted into the DB.
	assertModifiedTasks(t, d, trackId, []*db.Task{t1, t2, t3, t4, t5})
}

// AddTasks should call addTasksSingleTaskSpec for each group of tasks.
func TestAddTasks(t *testing.T) {
	gb, hashes, d, s, cleanup := setupAddTasksTest(t)
	defer cleanup()

	toil1 := makeTask("toil", gb.RepoUrl(), hashes[6])
	duty1 := makeTask("duty", gb.RepoUrl(), hashes[6])
	work1 := makeTask("work", gb.RepoUrl(), hashes[6])
	work2 := makeTask("work", gb.RepoUrl(), hashes[1])
	work2.Commits = []string{hashes[1], hashes[2], hashes[3], hashes[4], hashes[5]}
	sort.Strings(work2.Commits)
	onus1 := makeTask("onus", gb.RepoUrl(), hashes[6])
	onus2 := makeTask("onus", gb.RepoUrl(), hashes[3])
	onus2.Commits = []string{hashes[3], hashes[4], hashes[5]}
	sort.Strings(onus2.Commits)
	assert.NoError(t, d.PutTasks([]*db.Task{toil1, duty1, work1, work2, onus1, onus2}))

	trackId, err := d.StartTrackingModifiedTasks()
	assert.NoError(t, err)

	// toil2.Commits = {5}
	toil2 := makeTask("toil", gb.RepoUrl(), hashes[5])
	// toil3.Commits = {3, 4}
	toil3 := makeTask("toil", gb.RepoUrl(), hashes[3])

	// duty2.Commits = {1, 2, 3, 4, 5}
	duty2 := makeTask("duty", gb.RepoUrl(), hashes[1])
	// duty3.Commits = {3, 4, 5}
	// duty2.Commits = {1, 2}
	duty3 := makeTask("duty", gb.RepoUrl(), hashes[3])

	// work3.Commits = {3, 4, 5}
	// work2.Commits = {1, 2}
	work3 := makeTask("work", gb.RepoUrl(), hashes[3])
	// work4.Commits = {2}
	// work2.Commits = {1}
	work4 := makeTask("work", gb.RepoUrl(), hashes[2])

	onus2.Status = db.TASK_STATUS_MISHAP
	// onus3 steals all commits from onus2
	onus3 := makeTask("onus", gb.RepoUrl(), hashes[3])
	// onus4 steals all commits from onus3
	onus4 := makeTask("onus", gb.RepoUrl(), hashes[3])

	tasks := map[string]map[string][]*db.Task{
		gb.RepoUrl(): {
			"toil": {toil2, toil3},
			"duty": {duty2, duty3},
			"work": {work3, work4},
			"onus": {onus2, onus3, onus4},
		},
	}

	assert.NoError(t, s.AddTasks(tasks))

	assertBlamelist(t, hashes, toil2, []int{5})
	assertBlamelist(t, hashes, toil3, []int{3, 4})

	assertBlamelist(t, hashes, duty2, []int{1, 2})
	assertBlamelist(t, hashes, duty3, []int{3, 4, 5})

	work2Updated, err := d.GetTaskById(work2.Id)
	assert.NoError(t, err)
	assertBlamelist(t, hashes, work2Updated, []int{1})
	assertBlamelist(t, hashes, work3, []int{3, 4, 5})
	assertBlamelist(t, hashes, work4, []int{2})

	assertBlamelist(t, hashes, onus2, []int{})
	assertBlamelist(t, hashes, onus3, []int{})
	assertBlamelist(t, hashes, onus4, []int{3, 4, 5})

	// Check that the tasks were inserted into the DB.
	work2.Commits = work2Updated.Commits
	work2.DbModified = work2Updated.DbModified
	assertModifiedTasks(t, d, trackId, []*db.Task{toil2, toil3, duty2, duty3, work2, work3, work4, onus2, onus3, onus4})
}

// AddTasks should not leave DB in an inconsistent state if there is a partial error.
func TestAddTasksFailure(t *testing.T) {
	gb, hashes, d, s, cleanup := setupAddTasksTest(t)
	defer cleanup()

	toil1 := makeTask("toil", gb.RepoUrl(), hashes[6])
	duty1 := makeTask("duty", gb.RepoUrl(), hashes[6])
	duty2 := makeTask("duty", gb.RepoUrl(), hashes[5])
	assert.NoError(t, d.PutTasks([]*db.Task{toil1, duty1, duty2}))

	// Cause ErrConcurrentUpdate in AddTasks.
	cachedDuty2 := duty2.Copy()
	duty2.Status = db.TASK_STATUS_MISHAP
	assert.NoError(t, d.PutTask(duty2))

	trackId, err := d.StartTrackingModifiedTasks()
	assert.NoError(t, err)

	// toil2.Commits = {3, 4, 5}
	toil2 := makeTask("toil", gb.RepoUrl(), hashes[3])

	cachedDuty2.Status = db.TASK_STATUS_FAILURE
	// duty3.Commits = {3, 4}
	duty3 := makeTask("duty", gb.RepoUrl(), hashes[3])

	tasks := map[string]map[string][]*db.Task{
		gb.RepoUrl(): {
			"toil": {toil2},
			"duty": {cachedDuty2, duty3},
		},
	}

	// Try multiple times to reduce chance of test passing flakily.
	for i := 0; i < 3; i++ {
		err := s.AddTasks(tasks)
		assert.Error(t, err)
		modTasks, err := d.GetModifiedTasks(trackId)
		assert.NoError(t, err)
		// "duty" tasks should never be updated.
		for _, task := range modTasks {
			assert.Equal(t, "toil", task.Name)
			testutils.AssertDeepEqual(t, toil2, task)
			assertBlamelist(t, hashes, toil2, []int{3, 4, 5})
		}
	}

	duty2.Status = db.TASK_STATUS_FAILURE
	tasks[gb.RepoUrl()]["duty"] = []*db.Task{duty2, duty3}
	assert.NoError(t, s.AddTasks(tasks))

	assertBlamelist(t, hashes, toil2, []int{3, 4, 5})

	assertBlamelist(t, hashes, duty2, []int{5})
	assertBlamelist(t, hashes, duty3, []int{3, 4})

	// Check that the tasks were inserted into the DB.
	assertModifiedTasks(t, d, trackId, []*db.Task{toil2, duty2, duty3})
}

// AddTasks should retry on ErrConcurrentUpdate.
func TestAddTasksRetries(t *testing.T) {
	gb, hashes, d, s, cleanup := setupAddTasksTest(t)
	defer cleanup()

	toil1 := makeTask("toil", gb.RepoUrl(), hashes[6])
	duty1 := makeTask("duty", gb.RepoUrl(), hashes[6])
	work1 := makeTask("work", gb.RepoUrl(), hashes[6])
	toil2 := makeTask("toil", gb.RepoUrl(), hashes[1])
	toil2.Commits = []string{hashes[1], hashes[2], hashes[3], hashes[4], hashes[5]}
	sort.Strings(toil2.Commits)
	duty2 := makeTask("duty", gb.RepoUrl(), hashes[1])
	duty2.Commits = util.CopyStringSlice(toil2.Commits)
	work2 := makeTask("work", gb.RepoUrl(), hashes[1])
	work2.Commits = util.CopyStringSlice(toil2.Commits)
	assert.NoError(t, d.PutTasks([]*db.Task{toil1, toil2, duty1, duty2, work1, work2}))

	trackId, err := d.StartTrackingModifiedTasks()
	assert.NoError(t, err)

	// *3.Commits = {3, 4, 5}
	// *2.Commits = {1, 2}
	toil3 := makeTask("toil", gb.RepoUrl(), hashes[3])
	duty3 := makeTask("duty", gb.RepoUrl(), hashes[3])
	work3 := makeTask("work", gb.RepoUrl(), hashes[3])
	// *4.Commits = {2}
	// *2.Commits = {1}
	toil4 := makeTask("toil", gb.RepoUrl(), hashes[2])
	duty4 := makeTask("duty", gb.RepoUrl(), hashes[2])
	work4 := makeTask("work", gb.RepoUrl(), hashes[2])

	tasks := map[string]map[string][]*db.Task{
		gb.RepoUrl(): {
			"toil": {toil3.Copy(), toil4.Copy()},
			"duty": {duty3.Copy(), duty4.Copy()},
			"work": {work3.Copy(), work4.Copy()},
		},
	}

	retryCountMtx := sync.Mutex{}
	retryCount := map[string]int{}
	causeConcurrentUpdate := func(tasks []*db.Task) {
		retryCountMtx.Lock()
		defer retryCountMtx.Unlock()
		retryCount[tasks[0].Name]++
		if tasks[0].Name == "toil" && retryCount["toil"] < 2 {
			toil2.Started = time.Now().UTC()
			assert.NoError(t, d.PutTasks([]*db.Task{toil2}))
		}
		if tasks[0].Name == "duty" && retryCount["duty"] < 3 {
			duty2.Started = time.Now().UTC()
			assert.NoError(t, d.PutTasks([]*db.Task{duty2}))
		}
		if tasks[0].Name == "work" && retryCount["work"] < 4 {
			work2.Started = time.Now().UTC()
			assert.NoError(t, d.PutTasks([]*db.Task{work2}))
		}
	}
	s.db = &spyDB{
		DB:         d,
		onPutTasks: causeConcurrentUpdate,
	}

	assert.NoError(t, s.AddTasks(tasks))

	retryCountMtx.Lock()
	defer retryCountMtx.Unlock()
	assert.Equal(t, 2, retryCount["toil"])
	assert.Equal(t, 3, retryCount["duty"])
	assert.Equal(t, 4, retryCount["work"])

	modified := []*db.Task{}
	check := func(t2, t3, t4 *db.Task) {
		t2InDB, err := d.GetTaskById(t2.Id)
		assert.NoError(t, err)
		assertBlamelist(t, hashes, t2InDB, []int{1})
		t3Arg := tasks[t3.Repo][t3.Name][0]
		t3InDB, err := d.GetTaskById(t3Arg.Id)
		assert.NoError(t, err)
		testutils.AssertDeepEqual(t, t3Arg, t3InDB)
		assertBlamelist(t, hashes, t3InDB, []int{3, 4, 5})
		t4Arg := tasks[t4.Repo][t4.Name][1]
		t4InDB, err := d.GetTaskById(t4Arg.Id)
		assert.NoError(t, err)
		testutils.AssertDeepEqual(t, t4Arg, t4InDB)
		assertBlamelist(t, hashes, t4InDB, []int{2})
		t2.Commits = t2InDB.Commits
		t2.DbModified = t2InDB.DbModified
		t3.Id = t3InDB.Id
		t3.Commits = t3InDB.Commits
		t3.DbModified = t3InDB.DbModified
		t4.Id = t4InDB.Id
		t4.Commits = t4InDB.Commits
		t4.DbModified = t4InDB.DbModified
		modified = append(modified, t2, t3, t4)
	}

	check(toil2, toil3, toil4)
	check(duty2, duty3, duty4)
	check(work2, work3, work4)
	assertModifiedTasks(t, d, trackId, modified)
}

func TestValidateTaskForAdd(t *testing.T) {
	gb, _, _, s, _, cleanup := setup(t)
	defer cleanup()

	c1 := getRS1(t, gb).Revision

	test := func(task *db.Task, msg string) {
		err := s.ValidateAndAddTask(task)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), msg)
	}

	tmpl := makeTask("Fake-Name", gb.RepoUrl(), c1)
	tmpl.SwarmingTaskId = ""

	{
		task := tmpl.Copy()
		task.Id = "AlreadyExists"
		test(task, "Can not specify Id when adding task")
	}
	{
		task := tmpl.Copy()
		task.Name = ""
		test(task, "TaskKey is not valid")
	}
	{
		task := tmpl.Copy()
		task.SwarmingTaskId = "1"
		test(task, "Only fake tasks supported currently")
	}
	{
		task := tmpl.Copy()
		task.Revision = "abc123"
		test(task, "error: pathspec 'abc123' did not match any file(s) known to git.")
	}
	{
		task := tmpl.Copy()
		task.Name = specs_testutils.BuildTask
		test(task, "Can not add a fake task for a real task spec")
	}
	// Verify success.
	err := s.ValidateAndAddTask(tmpl)
	assert.NoError(t, err)
}

func TestAddTask(t *testing.T) {
	gb, d, _, s, _, cleanup := setup(t)
	defer cleanup()

	c1 := getRS1(t, gb).Revision
	c2 := getRS2(t, gb).Revision

	track, err := d.StartTrackingModifiedTasks()
	assert.NoError(t, err)

	// Add first task with Created set.
	task1 := makeTask("Fake-Name", gb.RepoUrl(), c2)
	task1.SwarmingTaskId = ""
	task1.Properties = map[string]string{
		"barnDoor": "open",
	}
	task1.Status = db.TASK_STATUS_SUCCESS
	expected1 := task1.Copy()
	err = s.ValidateAndAddTask(task1)
	assert.NoError(t, err)

	// Check updated task.
	assert.NotEqual(t, "", task1.Id)
	assert.False(t, util.TimeIsZero(task1.DbModified))
	expected1.Id = task1.Id
	expected1.DbModified = task1.DbModified
	expected1.Commits = []string{c2, c1}
	testutils.AssertDeepEqual(t, expected1, task1)

	// Add commits on master.
	makeDummyCommits(gb, 2)
	assert.NoError(t, s.updateRepos())
	commits, err := s.repos[gb.RepoUrl()].Repo().RevList("HEAD")
	assert.NoError(t, err)

	// Add second task with Created unset; Commits set incorrectly.
	task2 := makeTask("Fake-Name", gb.RepoUrl(), commits[0])
	task2.SwarmingTaskId = ""
	task2.Properties = map[string]string{
		"barnDoor": "closed",
	}
	task2.Status = db.TASK_STATUS_RUNNING
	task2.Commits = []string{c1, c2}
	expected2 := task2.Copy()
	err = s.ValidateAndAddTask(task2)
	assert.NoError(t, err)

	// Check updated task.
	assert.NotEqual(t, "", task2.Id)
	assert.False(t, util.TimeIsZero(task2.DbModified))
	assert.False(t, util.TimeIsZero(task2.Created))
	expected2.Id = task2.Id
	expected2.DbModified = task2.DbModified
	expected2.Created = task2.Created
	expected2.Commits = []string{commits[0], commits[1]}
	sort.Strings(task2.Commits)
	sort.Strings(expected2.Commits)
	testutils.AssertDeepEqual(t, expected2, task2)

	// Backfill.
	task3 := makeTask("Fake-Name", gb.RepoUrl(), commits[1])
	task3.SwarmingTaskId = ""
	task3.Properties = map[string]string{
		"barnDoor": "closed",
	}
	task3.Status = db.TASK_STATUS_MISHAP
	expected3 := task3.Copy()
	err = s.ValidateAndAddTask(task3)
	assert.NoError(t, err)

	// Check updated task.
	assert.NotEqual(t, "", task3.Id)
	assert.False(t, util.TimeIsZero(task3.DbModified))
	expected3.Id = task3.Id
	expected3.DbModified = task3.DbModified
	expected3.Commits = []string{commits[1]}
	testutils.AssertDeepEqual(t, expected3, task3)

	// Check DB.
	tasks, err := d.GetModifiedTasks(track)
	assert.NoError(t, err)
	var actual1, actual2, actual3 *db.Task
	for _, task := range tasks {
		switch task.Id {
		case task1.Id:
			actual1 = task
		case task2.Id:
			actual2 = task
		case task3.Id:
			actual3 = task
		}
	}
	testutils.AssertDeepEqual(t, expected1, actual1)
	assert.True(t, actual2.DbModified.After(task2.DbModified))
	expected2.DbModified = actual2.DbModified
	expected2.Commits = []string{commits[0]}
	testutils.AssertDeepEqual(t, expected2, actual2)
	testutils.AssertDeepEqual(t, expected3, actual3)
}

func TestValidateTaskForUpdate(t *testing.T) {
	testutils.SmallTest(t)

	d := db.NewInMemoryDB()

	c1 := "abc123"
	c2 := "def456"

	orig := makeTask("Fake-Name", "skia.git", c1)
	orig.SwarmingTaskId = ""
	orig.Commits = []string{c1, c2}
	assert.NoError(t, d.PutTask(orig))

	real := makeTask(specs_testutils.BuildTask, "skia.git", c1)
	assert.NoError(t, d.PutTask(real))

	test := func(task *db.Task, msg string) {
		err := validateAndUpdateTask(d, task)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), msg)
	}

	{
		task := orig.Copy()
		task.Id = ""
		test(task, "Must specify Id when updating task")
	}
	{
		task := orig.Copy()
		task.Name = ""
		test(task, "TaskKey is not valid")
	}
	{
		task := orig.Copy()
		task.SwarmingTaskId = "1"
		test(task, "Only fake tasks supported currently")
	}
	{
		task := orig.Copy()
		task.Id = "abc123"
		test(task, "No such task")
	}
	{
		task := real.Copy()
		task.SwarmingTaskId = ""
		test(task, "Can not overwrite real task with fake task")
	}
	{
		task := orig.Copy()
		task.DbModified = task.DbModified.Add(-time.Minute)
		err := validateAndUpdateTask(d, task)
		assert.Error(t, err)
		assert.True(t, db.IsConcurrentUpdate(err))
	}
	{
		task := orig.Copy()
		task.Created = task.Created.Add(time.Minute)
		test(task, "Illegal update")
	}
	{
		task := orig.Copy()
		task.Revision = "bbad"
		test(task, "Illegal update")
	}
	{
		task := orig.Copy()
		task.Name = specs_testutils.BuildTask
		test(task, "Illegal update")
	}
	{
		task := orig.Copy()
		task.Commits = []string{c1}
		test(task, "Illegal update")
	}
	// Verify success.
	assert.NoError(t, validateAndUpdateTask(d, orig))
}

func TestUpdateTask(t *testing.T) {
	testutils.SmallTest(t)

	d := db.NewInMemoryDB()

	c1 := "abc123"
	c2 := "def456"
	task := makeTask("Fake-Name", "skia.git", c2)
	task.SwarmingTaskId = ""
	task.Commits = []string{c1, c2}
	task.Properties = map[string]string{
		"barnDoor": "open",
	}
	task.Started = time.Now()
	task.Status = db.TASK_STATUS_RUNNING
	assert.NoError(t, d.PutTask(task))

	track, err := d.StartTrackingModifiedTasks()
	assert.NoError(t, err)

	task.Commits = []string{c2, c1}
	task.Finished = time.Now()
	task.Properties["barnDoor"] = "closed"
	task.Properties["yourHorsesHeld"] = "true"
	task.Started = time.Now().Add(-time.Minute)
	task.Status = db.TASK_STATUS_SUCCESS
	expected := task.Copy()

	assert.NoError(t, validateAndUpdateTask(d, task))

	assert.True(t, expected.DbModified.Before(task.DbModified))
	expected.DbModified = task.DbModified
	testutils.AssertDeepEqual(t, expected, task)

	// Check DB.
	tasks, err := d.GetModifiedTasks(track)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(tasks))
	testutils.AssertDeepEqual(t, expected, tasks[0])
}

func TestTriggerTaskFailed(t *testing.T) {
	// Verify that if one task out of a set fails to trigger, the others are
	// still inserted into the DB and handled properly, eg. wrt. blamelists.
	gb, _, s, swarmingClient, commits, _, cleanup := testMultipleCandidatesBackfillingEachOtherSetup(t)
	defer cleanup()

	// Trigger three tasks. We should attempt to trigger tasks at
	// commits[0], commits[4], and either commits[2] or commits[6]. Mock
	// failure to trigger the task at commits[4] and ensure that the other
	// two tasks get inserted with the correct blamelists.
	bot1 := makeBot("bot1", map[string]string{"pool": "Skia"})
	bot2 := makeBot("bot2", map[string]string{"pool": "Skia"})
	bot3 := makeBot("bot3", map[string]string{"pool": "Skia"})
	makeTags := func(commit string) []string {
		return []string{
			"luci_project:",
			"sk_attempt:0",
			"sk_dim_pool:Skia",
			"sk_retry_of:",
			fmt.Sprintf("source_revision:%s", commit),
			fmt.Sprintf("source_repo:%s/+/%%s", gb.RepoUrl()),
			fmt.Sprintf("sk_repo:%s", gb.RepoUrl()),
			fmt.Sprintf("sk_revision:%s", commit),
			"sk_forced_job_id:",
			"sk_name:dummytask",
			"sk_priority:1.000000",
		}
	}
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2, bot3})
	swarmingClient.MockTriggerTaskFailure(makeTags(commits[4]))
	err := s.MainLoop()
	assert.EqualError(t, err, "Got failures: \nFailed to trigger task: Mocked trigger failure!\n")
	assert.NoError(t, s.tCache.Update())
	assert.Equal(t, 6, len(s.queue))
	tasks, err := s.tCache.GetTasksForCommits(gb.RepoUrl(), commits)
	assert.NoError(t, err)

	var t1, t2, t3 *db.Task
	for _, byName := range tasks {
		for _, task := range byName {
			if task.Revision == commits[0] {
				t1 = task
			} else if task.Revision == commits[4] {
				t2 = task
			} else if task.Revision == commits[2] || task.Revision == commits[6] {
				t3 = task
			} else {
				assert.FailNow(t, fmt.Sprintf("Task has unknown revision: %v", task))
			}
		}
	}
	assert.NotNil(t, t1)
	assert.Nil(t, t2)
	assert.NotNil(t, t3)

	// Ensure that we got the blamelists right.
	var expect1, expect3 []string
	if t3.Revision == commits[2] {
		expect1 = util.CopyStringSlice(commits[:2])
		expect3 = util.CopyStringSlice(commits[2:])
	} else {
		expect1 = util.CopyStringSlice(commits[:6])
		expect3 = util.CopyStringSlice(commits[6:])
	}
	sort.Strings(expect1)
	sort.Strings(expect3)
	sort.Strings(t1.Commits)
	sort.Strings(t3.Commits)
	testutils.AssertDeepEqual(t, expect1, t1.Commits)
	testutils.AssertDeepEqual(t, expect3, t3.Commits)
}

func TestIsolateTaskFailed(t *testing.T) {
	// Verify that if one task out of a set fails to isolate, the others are
	// still triggered, inserted into the DB, etc.
	gb, _, s, swarmingClient, commits, _, cleanup := testMultipleCandidatesBackfillingEachOtherSetup(t)
	defer cleanup()

	bot1 := makeBot("bot1", map[string]string{"pool": "Skia"})
	bot2 := makeBot("bot2", map[string]string{"pool": "Skia"})
	swarmingClient.MockBots([]*swarming_api.SwarmingRpcsBotInfo{bot1, bot2})

	// Create a new commit with a bad isolate.
	gb.Add(path.Join("infra", "bots", "dummy.isolate"), `sadkldsafkldsafkl30909098]]]]];;0`)
	badCommit := gb.Commit()

	// Create a commit which fixes the bad isolate.
	gb.Add(path.Join("infra", "bots", "dummy.isolate"), `{
  'variables': {
    'command': [
      'python', 'recipes.py', 'run',
    ],
    'files': [
      '../../somefile.txt',
    ],
  },
}`)
	fix := gb.Commit()

	commits = append([]string{fix, badCommit}, commits...)

	// Now we have 9 untested commits. Add 8 more to put the bad commit
	// right in the middle.
	for i := 0; i < 8; i++ {
		commits = append([]string{gb.CommitGen("dummyfile")}, commits...)
	}

	// We should run at tip-of-tree, and attempt to run at the bad commit.
	err := s.MainLoop()
	assert.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "isolate: failed to process isolate"))
	assert.NoError(t, s.tCache.Update())
	assert.Equal(t, 17, len(s.queue))
	assert.Equal(t, badCommit, s.queue[0].Revision)
	tasks, err := s.tCache.GetTasksForCommits(gb.RepoUrl(), commits)
	assert.NoError(t, err)

	var t1 *db.Task
	for _, byName := range tasks {
		for _, task := range byName {
			if task.Revision == commits[0] {
				t1 = task
			} else {
				assert.FailNow(t, fmt.Sprintf("Task has unknown revision %s: %+v", task.Revision, task))
			}
		}
	}
	assert.NotNil(t, t1)

	// Ensure that we got the blamelists right.
	expect1 := util.CopyStringSlice(commits)
	sort.Strings(expect1)
	sort.Strings(t1.Commits)
	testutils.AssertDeepEqual(t, expect1, t1.Commits)
}
