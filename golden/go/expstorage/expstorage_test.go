package expstorage

import (
	"sort"
	"testing"
)

import (
	assert "github.com/stretchr/testify/require"
	"go.skia.org/infra/go/database/testutil"
	"go.skia.org/infra/go/eventbus"
	"go.skia.org/infra/go/util"
	"go.skia.org/infra/golden/go/db"
	"go.skia.org/infra/golden/go/types"
)

func TestMySQLExpectationsStore(t *testing.T) {
	// Set up the test database.
	testDb := testutil.SetupMySQLTestDatabase(t, db.MigrationSteps())
	defer testDb.Close(t)

	conf := testutil.LocalTestDatabaseConfig(db.MigrationSteps())
	vdb, err := conf.NewVersionedDB()
	assert.Nil(t, err)

	// Test the MySQL backed store
	sqlStore := NewSQLExpectationStore(vdb)
	testExpectationStore(t, sqlStore, nil)

	// Test the caching version of the MySQL store.
	eventBus := eventbus.New()
	cachingStore := NewCachingExpectationStore(sqlStore, eventBus)
	testExpectationStore(t, cachingStore, eventBus)
}

// Test against the expectation store interface.
func testExpectationStore(t *testing.T, store ExpectationsStore, eventBus *eventbus.EventBus) {
	// Get the initial log size. This is necessary because we
	// call this function multiple times with the same underlying
	// SQLExpectationStore.
	initialLogRecs, initialLogTotal, err := store.QueryLog(0, 5)
	assert.Nil(t, err)
	initialLogRecsLen := len(initialLogRecs)

	// If we have an event bus then keep gathering events.
	callbackCh := make(chan []string, 3)
	if eventBus != nil {
		eventBus.SubscribeAsync(EV_EXPSTORAGE_CHANGED, func(e interface{}) {
			testNames := append([]string{}, e.([]string)...)
			sort.Strings(testNames)
			callbackCh <- testNames
		})
	}

	TEST_1, TEST_2 := "test1", "test2"

	// digests
	DIGEST_11, DIGEST_12 := "d11", "d12"
	DIGEST_21, DIGEST_22 := "d21", "d22"

	newExps := map[string]types.TestClassification{
		TEST_1: types.TestClassification{
			DIGEST_11: types.POSITIVE,
			DIGEST_12: types.NEGATIVE,
		},
		TEST_2: types.TestClassification{
			DIGEST_21: types.POSITIVE,
			DIGEST_22: types.NEGATIVE,
		},
	}
	assert.Nil(t, store.AddChange(newExps, "user-0"))
	if eventBus != nil {
		eventBus.Wait(EV_EXPSTORAGE_CHANGED)
		assert.Equal(t, 1, len(callbackCh))
		assert.Equal(t, []string{TEST_1, TEST_2}, <-callbackCh)
	}

	foundExps, err := store.Get()
	assert.Nil(t, err)

	assert.Equal(t, newExps, foundExps.Tests)
	assert.False(t, &newExps == &foundExps.Tests)

	// Update digests.
	updExps := map[string]types.TestClassification{
		TEST_1: types.TestClassification{
			DIGEST_11: types.NEGATIVE,
		},
		TEST_2: types.TestClassification{
			DIGEST_22: types.UNTRIAGED,
		},
	}
	assert.Nil(t, store.AddChange(updExps, "user-1"))
	if eventBus != nil {
		eventBus.Wait(EV_EXPSTORAGE_CHANGED)
		assert.Equal(t, 1, len(callbackCh))
		assert.Equal(t, []string{TEST_1, TEST_2}, <-callbackCh)
	}

	foundExps, err = store.Get()
	assert.Nil(t, err)
	assert.Equal(t, types.NEGATIVE, foundExps.Tests[TEST_1][DIGEST_11])
	assert.Equal(t, types.UNTRIAGED, foundExps.Tests[TEST_2][DIGEST_22])

	// Send empty changes to test the event bus.
	emptyChanges := map[string]types.TestClassification{}
	assert.Nil(t, store.AddChange(emptyChanges, ""))
	if eventBus != nil {
		eventBus.Wait(EV_EXPSTORAGE_CHANGED)
		assert.Equal(t, 1, len(callbackCh))
		assert.Equal(t, []string{}, <-callbackCh)
	}

	// Remove digests.
	removeDigests := map[string][]string{
		TEST_1: []string{DIGEST_11},
		TEST_2: []string{DIGEST_22},
	}
	assert.Nil(t, store.RemoveChange(removeDigests))
	if eventBus != nil {
		eventBus.Wait(EV_EXPSTORAGE_CHANGED)
		assert.Equal(t, 1, len(callbackCh))
		assert.Equal(t, []string{TEST_1, TEST_2}, <-callbackCh)
	}

	foundExps, err = store.Get()
	assert.Nil(t, err)

	assert.Equal(t, types.TestClassification(map[string]types.Label{DIGEST_12: types.NEGATIVE}), foundExps.Tests[TEST_1])
	assert.Equal(t, types.TestClassification(map[string]types.Label{DIGEST_21: types.POSITIVE}), foundExps.Tests[TEST_2])

	assert.Nil(t, store.RemoveChange(map[string][]string{TEST_1: []string{DIGEST_12}}))
	if eventBus != nil {
		eventBus.Wait(EV_EXPSTORAGE_CHANGED)
		assert.Equal(t, 1, len(callbackCh))
		assert.Equal(t, []string{TEST_1}, <-callbackCh)
	}

	foundExps, err = store.Get()
	assert.Nil(t, err)
	assert.Equal(t, 1, len(foundExps.Tests))

	assert.Nil(t, store.RemoveChange(map[string][]string{}))
	if eventBus != nil {
		eventBus.Wait(EV_EXPSTORAGE_CHANGED)
		assert.Equal(t, 1, len(callbackCh))
		assert.Equal(t, []string{}, <-callbackCh)
	}

	// Make sure we added the correct number of triage log entries.
	logEntries, total, err := store.QueryLog(0, 5)
	assert.Nil(t, err)
	assert.Equal(t, 3+initialLogTotal, total)
	assert.Equal(t, util.MinInt(3+initialLogRecsLen, 5), len(logEntries))

	logEntries, total, err = store.QueryLog(100, 5)
	assert.Nil(t, err)
	assert.Equal(t, 3+initialLogTotal, total)
	assert.Equal(t, 0, len(logEntries))
}
