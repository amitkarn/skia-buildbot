package blacklist

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sync"

	"github.com/skia-dev/glog"

	"go.skia.org/infra/go/buildbot"
	"go.skia.org/infra/go/git/repograph"
	"go.skia.org/infra/go/util"
)

const (
	MAX_NAME_CHARS = 50
)

var (
	DEFAULT_RULES = []*Rule{
		&Rule{
			AddedBy: "TaskScheduler",
			TaskSpecPatterns: []string{
				buildbot.TRYBOT_PATTERN,
			},
			Commits:     []string{},
			Description: "Trybots are scheduled through Rietveld or the Commit Queue.",
			Name:        "Trybots",
		},
		&Rule{
			AddedBy: "TaskScheduler",
			TaskSpecPatterns: []string{
				"^Housekeeper-Nightly-RecreateSKPs_Canary$",
				"^Housekeeper-Weekly-RecreateSKPs$",
				"^Test-Ubuntu-GCC-GCE-CPU-AVX2-x86_64-Debug-CT_DM_1m_SKPs$",
			},
			Commits:     []string{},
			Description: "Bots which the Task Scheduler should not schedule because they run on a timer.",
			Name:        "Timed Bots",
		},
		&Rule{
			AddedBy: "TaskScheduler",
			TaskSpecPatterns: []string{
				"^Infra-PerCommit$",
			},
			Commits:     []string{"355d0d378d1b9f2df9abe9fd4a73348d9b13471b"},
			Description: "Infra-PerCommit is broken at this revision.",
			Name:        "Infra-PerCommit@355d0d3",
		},
	}

	ERR_NO_SUCH_RULE = fmt.Errorf("No such rule.")
)

// Blacklist is a struct which contains rules specifying tasks which should
// not be scheduled.
type Blacklist struct {
	backingFile string
	Rules       map[string]*Rule `json:"rules"`
	mtx         sync.RWMutex
}

// Match determines whether the given taskSpec/commit pair matches one of the
// Rules in the Blacklist.
func (b *Blacklist) Match(taskSpec, commit string) bool {
	return b.MatchRule(taskSpec, commit) != ""
}

// MatchRule determines whether the given taskSpec/commit pair matches one of the
// Rules in the Blacklist. Returns the name of the matched Rule or the empty
// string if no Rules match.
func (b *Blacklist) MatchRule(taskSpec, commit string) string {
	b.mtx.RLock()
	defer b.mtx.RUnlock()
	for _, rule := range b.Rules {
		if rule.Match(taskSpec, commit) {
			return rule.Name
		}
	}
	return ""
}

// ensureDefaults adds the necessary default blacklist rules if necessary.
func (b *Blacklist) ensureDefaults() error {
	for _, rule := range DEFAULT_RULES {
		if err := b.removeRule(rule.Name); err != nil {
			if err.Error() != ERR_NO_SUCH_RULE.Error() {
				return err
			}
		}
		if err := b.addRule(rule); err != nil {
			return err
		}
	}
	return nil
}

// writeOut writes the Blacklist to its backing file. Assumes that the caller
// holds a write lock.
func (b *Blacklist) writeOut() error {
	f, err := os.Create(b.backingFile)
	if err != nil {
		return err
	}
	defer util.Close(f)
	return json.NewEncoder(f).Encode(b)
}

// Add adds a new Rule to the Blacklist.
func (b *Blacklist) AddRule(r *Rule, repos repograph.Map) error {
	if err := ValidateRule(r, repos); err != nil {
		return err
	}
	return b.addRule(r)
}

// addRule adds a new Rule to the Blacklist.
func (b *Blacklist) addRule(r *Rule) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	if _, ok := b.Rules[r.Name]; ok {
		return fmt.Errorf("Blacklist already contains a rule named %q", r.Name)
	}
	b.Rules[r.Name] = r
	if err := b.writeOut(); err != nil {
		delete(b.Rules, r.Name)
		return err
	}
	return nil
}

// findCommit finds the repo for the given commit.
func findCommit(c string, repos repograph.Map) (string, error) {
	for repoName, r := range repos {
		commit := r.Get(c)
		if commit != nil && commit.Hash == c {
			return repoName, nil
		}
	}
	return "", fmt.Errorf("Could not find commit %s in any repo.", c)
}

// NewCommitRangeRule creates a new Rule which covers a range of commits.
func NewCommitRangeRule(name, user, description string, taskSpecPatterns []string, startCommit, endCommit string, repos repograph.Map) (*Rule, error) {
	repoName, err := findCommit(startCommit, repos)
	if err != nil {
		return nil, err
	}
	repo2, err := findCommit(endCommit, repos)
	if err != nil {
		return nil, err
	}
	if repo2 != repoName {
		return nil, fmt.Errorf("Commit %s is in a different repo (%s) from %s (%s)", endCommit, repo2, startCommit, repoName)
	}
	repo, ok := repos[repoName]
	if !ok {
		return nil, fmt.Errorf("Unknown repo %s", repoName)
	}
	commits, err := repo.Repo().RevList(fmt.Sprintf("%s..%s", startCommit, endCommit))
	if err != nil {
		return nil, err
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("No commits in range %s..%s", startCommit, endCommit)
	}

	// `git rev-list ${startCommit}..${endCommit}` returns a list of commits
	// which does not include startCommit but does include endCommit. For
	// blacklisting rules, we want to include startCommit and not endCommit.
	// The rev-list command returns commits in order of newest to oldest, so
	// we remove the first element of the slice (endCommit), and append
	// startCommit to the end.
	commits = append(commits[1:], startCommit)
	if util.In(endCommit, commits) {
		return nil, fmt.Errorf("Failed to adjust commit range; still includes endCommit.")
	}
	if !util.In(startCommit, commits) {
		return nil, fmt.Errorf("Failed to adjust commit range; does not include startCommit.")
	}

	rule := &Rule{
		AddedBy:          user,
		TaskSpecPatterns: taskSpecPatterns,
		Commits:          commits,
		Description:      description,
		Name:             name,
	}
	if err := ValidateRule(rule, repos); err != nil {
		return nil, err
	}
	return rule, nil
}

// removeRule removes the Rule from the Blacklist.
func (b *Blacklist) removeRule(name string) error {
	b.mtx.Lock()
	defer b.mtx.Unlock()
	r, ok := b.Rules[name]
	if !ok {
		return ERR_NO_SUCH_RULE
	}
	delete(b.Rules, name)
	if err := b.writeOut(); err != nil {
		b.Rules[name] = r
		return err
	}
	return nil
}

// RemoveRule removes the Rule from the Blacklist.
func (b *Blacklist) RemoveRule(name string) error {
	for _, r := range DEFAULT_RULES {
		if r.Name == name {
			return fmt.Errorf("Cannot remove built-in rule %q", name)
		}
	}
	return b.removeRule(name)
}

// Rule is a struct which indicates a specific task or set of tasks which
// should not be scheduled.
//
// TaskSpecPatterns consists of regular expressions used to match taskSpecs
// which should not be triggered according to this Rule.
//
// Commits are simply commit hashes for which the rule applies. If the list is
// empty, the Rule applies for all commits.
//
// A Rule should specify TaskSpecPatterns or Commits or both.
type Rule struct {
	AddedBy          string   `json:"added_by"`
	TaskSpecPatterns []string `json:"task_spec_patterns"`
	Commits          []string `json:"commits"`
	Description      string   `json:"description"`
	Name             string   `json:"name"`
}

// ValidateRule returns an error if the given Rule is not valid.
func ValidateRule(r *Rule, repos repograph.Map) error {
	if r.Name == "" {
		return fmt.Errorf("Rules must have a name.")
	}
	if len(r.Name) > MAX_NAME_CHARS {
		return fmt.Errorf("Rule names must be shorter than %d characters. Use the Description field for detailed information.", MAX_NAME_CHARS)
	}
	if r.AddedBy == "" {
		return fmt.Errorf("Rules must have an AddedBy user.")
	}
	if len(r.TaskSpecPatterns) == 0 && len(r.Commits) == 0 {
		return fmt.Errorf("Rules must include a taskSpec pattern and/or a commit/range.")
	}
	for _, c := range r.Commits {
		if _, err := findCommit(c, repos); err != nil {
			return err
		}
	}
	return nil
}

// matchTaskSpec determines whether the taskSpec portion of the Rule matches.
func (r *Rule) matchTaskSpec(taskSpec string) bool {
	// If no taskSpecs are specified, then the rule applies for ALL taskSpecs.
	if len(r.TaskSpecPatterns) == 0 {
		return true
	}
	// If any pattern matches the taskSpec, then the rule applies.
	for _, b := range r.TaskSpecPatterns {
		match, err := regexp.MatchString(b, taskSpec)
		if err != nil {
			glog.Warningf("Rule regexp returned error for input %q: %s: %s", taskSpec, b, err)
			return false
		}
		if match {
			return true
		}
	}
	return false
}

// matchCommit determines whether the commit portion of the Rule matches.
func (r *Rule) matchCommit(commit string) bool {
	// If no commit is specified, then the rule applies for ALL commits.
	k := len(r.Commits)
	if k == 0 {
		return true
	}
	// If at least one commit is specified, do simple string comparisons.
	for _, c := range r.Commits {
		if commit == c {
			return true
		}
	}
	return false
}

// Match returns true iff the Rule matches the given taskSpec and commit.
func (r *Rule) Match(taskSpec, commit string) bool {
	return r.matchTaskSpec(taskSpec) && r.matchCommit(commit)
}

// FromFile returns a Blacklist instance based on the given file. If the file
// does not exist, the Blacklist will be empty and will attempt to use the file
// for writing.
func FromFile(file string) (*Blacklist, error) {
	b := &Blacklist{
		backingFile: file,
		mtx:         sync.RWMutex{},
	}
	f, err := os.Open(file)
	if err != nil {
		if os.IsNotExist(err) {
			b.Rules = map[string]*Rule{}
			if err := b.writeOut(); err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	} else {
		defer util.Close(f)
		if err := json.NewDecoder(f).Decode(b); err != nil {
			return nil, err
		}
	}
	if err := b.ensureDefaults(); err != nil {
		return nil, err
	}
	return b, nil
}
