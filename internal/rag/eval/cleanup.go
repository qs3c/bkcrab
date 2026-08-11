package eval

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
)

const (
	defaultEvalCleanupInterval = 10 * time.Minute
	defaultStagingRetention    = 24 * time.Hour
)

type CleanupClock interface{ Now() time.Time }

type systemCleanupClock struct{}

func (systemCleanupClock) Now() time.Time { return time.Now() }

type CleanupStore interface {
	ListRAGEvalRunGCCandidates(context.Context, time.Time, int) ([]string, error)
	PurgeRAGEvalRun(context.Context, string) (bool, error)
}

type DatasetCleaner interface {
	CleanupStaging(context.Context, time.Time, int) (int, error)
	GarbageCollect(context.Context, time.Time, int) (int, error)
}

type GenerationCleaner interface {
	GarbageCollect(context.Context, time.Time, int) (int, error)
}

type CleanupResult struct {
	Staging, Runs, Datasets, Generations int
}

// CleanupCoordinator is a bounded, restart-safe sweeper. SQL selects every
// physical cleanup target first; the individual cleaners then enforce their
// own namespace and fence checks before deleting anything outside SQL.
type CleanupCoordinator struct {
	store       CleanupStore
	datasets    DatasetCleaner
	generations GenerationCleaner
	clock       CleanupClock
	interval    time.Duration
	runTTL      time.Duration
	datasetTTL  time.Duration
	batch       int
}

func NewCleanupCoordinator(st CleanupStore, datasets DatasetCleaner, generations GenerationCleaner, cfg config.RAGEvaluationCfg, clock CleanupClock) (*CleanupCoordinator, error) {
	if st == nil || datasets == nil || generations == nil {
		return nil, errors.New("evaluation cleanup dependencies are required")
	}
	cfg.ApplyDefaults()
	if clock == nil {
		clock = systemCleanupClock{}
	}
	return &CleanupCoordinator{store: st, datasets: datasets, generations: generations, clock: clock,
		interval: defaultEvalCleanupInterval, runTTL: time.Duration(cfg.RunRetentionDays) * 24 * time.Hour,
		datasetTTL: time.Duration(cfg.DatasetRetentionDays) * 24 * time.Hour, batch: 50}, nil
}

func (c *CleanupCoordinator) RunOnce(ctx context.Context) (CleanupResult, error) {
	if c == nil {
		return CleanupResult{}, errors.New("evaluation cleanup is unavailable")
	}
	now := c.clock.Now().UTC()
	result := CleanupResult{}
	var err error
	if result.Staging, err = c.datasets.CleanupStaging(ctx, now.Add(-defaultStagingRetention), c.batch); err != nil {
		return result, err
	}
	candidates, err := c.store.ListRAGEvalRunGCCandidates(ctx, now.Add(-c.runTTL), c.batch)
	if err != nil {
		return result, err
	}
	for _, id := range candidates {
		purged, purgeErr := c.store.PurgeRAGEvalRun(ctx, id)
		if purgeErr != nil {
			return result, purgeErr
		}
		if purged {
			result.Runs++
		}
	}
	if result.Datasets, err = c.datasets.GarbageCollect(ctx, now.Add(-c.datasetTTL), c.batch); err != nil {
		return result, err
	}
	// Generation rows carry their own expires_at rollback window. Passing now
	// never shortens that configured retention.
	if result.Generations, err = c.generations.GarbageCollect(ctx, now, c.batch); err != nil {
		return result, err
	}
	return result, nil
}

func (c *CleanupCoordinator) Start(ctx context.Context) {
	if c == nil {
		return
	}
	go func() {
		run := func() {
			if result, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
				slog.WarnContext(ctx, "rag eval cleanup failed", "error", err)
			} else if result != (CleanupResult{}) {
				slog.InfoContext(ctx, "rag eval cleanup complete", "staging", result.Staging, "runs", result.Runs, "datasets", result.Datasets, "generations", result.Generations)
			}
		}
		run()
		ticker := time.NewTicker(c.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				run()
			}
		}
	}()
}
