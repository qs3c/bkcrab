package eval

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
)

type fakeCleanupClock struct{ now time.Time }

func (c fakeCleanupClock) Now() time.Time { return c.now }

type fakeCleanupStore struct {
	before  time.Time
	ids     []string
	purged  []string
	failFor string
}

func (s *fakeCleanupStore) ListRAGEvalRunGCCandidates(_ context.Context, before time.Time, _ int) ([]string, error) {
	s.before = before
	return append([]string(nil), s.ids...), nil
}
func (s *fakeCleanupStore) PurgeRAGEvalRun(_ context.Context, id string) (bool, error) {
	if id == s.failFor {
		return false, errors.New("interrupted")
	}
	s.purged = append(s.purged, id)
	return true, nil
}

type fakeDatasetCleaner struct{ stagingBefore, gcBefore time.Time }

func (c *fakeDatasetCleaner) CleanupStaging(_ context.Context, before time.Time, _ int) (int, error) {
	c.stagingBefore = before
	return 1, nil
}
func (c *fakeDatasetCleaner) GarbageCollect(_ context.Context, before time.Time, _ int) (int, error) {
	c.gcBefore = before
	return 2, nil
}

type fakeGenerationCleaner struct{ before time.Time }

func (c *fakeGenerationCleaner) GarbageCollect(_ context.Context, before time.Time, _ int) (int, error) {
	c.before = before
	return 3, nil
}

func TestCleanupCoordinatorUsesConfiguredRetentionWithFakeClock(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	st := &fakeCleanupStore{ids: []string{"run-old"}}
	datasets, generations := &fakeDatasetCleaner{}, &fakeGenerationCleaner{}
	coordinator, err := NewCleanupCoordinator(st, datasets, generations, config.RAGEvaluationCfg{RunRetentionDays: 7, DatasetRetentionDays: 31}, fakeCleanupClock{now})
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.RunOnce(context.Background())
	if err != nil || result != (CleanupResult{Staging: 1, Runs: 1, Datasets: 2, Generations: 3}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !st.before.Equal(now.Add(-7*24*time.Hour)) || !datasets.gcBefore.Equal(now.Add(-31*24*time.Hour)) ||
		!datasets.stagingBefore.Equal(now.Add(-24*time.Hour)) || !generations.before.Equal(now) {
		t.Fatalf("cutoffs run=%v staging=%v dataset=%v generation=%v", st.before, datasets.stagingBefore, datasets.gcBefore, generations.before)
	}
}

func TestCleanupCoordinatorCanResumeAfterInterruption(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	st := &fakeCleanupStore{ids: []string{"run-one", "run-two"}, failFor: "run-two"}
	coordinator, _ := NewCleanupCoordinator(st, &fakeDatasetCleaner{}, &fakeGenerationCleaner{}, config.RAGEvaluationCfg{}, fakeCleanupClock{now})
	if _, err := coordinator.RunOnce(context.Background()); err == nil || !reflect.DeepEqual(st.purged, []string{"run-one"}) {
		t.Fatalf("first pass purged=%v err=%v", st.purged, err)
	}
	// SQL candidate discovery is authoritative on every pass. In production the
	// first row is already gone; emulate that state and retry the interrupted row.
	st.ids, st.failFor, st.purged = []string{"run-two"}, "", nil
	result, err := coordinator.RunOnce(context.Background())
	if err != nil || result.Runs != 1 || !reflect.DeepEqual(st.purged, []string{"run-two"}) {
		t.Fatalf("resume result=%+v purged=%v err=%v", result, st.purged, err)
	}
}
