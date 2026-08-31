package parseeval

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/store"
)

type CleanupStore interface {
	GetParserEvalRun(context.Context, string) (*store.ParserEvalRunRecord, error)
	ListExpiredParserEvalRuns(context.Context, time.Time, int) ([]store.ParserEvalRunRecord, error)
	PurgeParserEvalRun(context.Context, string) (bool, error)
}

type Cleanup struct {
	store   CleanupStore
	objects objects.Store
	now     func() time.Time
}

func NewCleanup(database CleanupStore, objectStore objects.Store) (*Cleanup, error) {
	if database == nil || objectStore == nil {
		return nil, errors.New("parser evaluation cleanup store and object store are required")
	}
	return &Cleanup{store: database, objects: objectStore, now: func() time.Time { return time.Now().UTC() }}, nil
}

// CleanupExpired deletes object prefixes before purging the corresponding two
// SQL-table records. A failed object deletion leaves SQL intact for retry.
func (c *Cleanup) CleanupExpired(ctx context.Context, limit int) (int, error) {
	runs, err := c.store.ListExpiredParserEvalRuns(ctx, c.now(), limit)
	if err != nil {
		return 0, err
	}
	removed := 0
	failures := make([]error, 0)
	for _, run := range runs {
		if err := c.deleteTerminal(ctx, run); err != nil {
			failures = append(failures, fmt.Errorf("cleanup parser evaluation %s: %w", run.ID, err))
			continue
		}
		removed++
	}
	return removed, errors.Join(failures...)
}

func (c *Cleanup) DeleteRun(ctx context.Context, runID string) error {
	run, err := c.store.GetParserEvalRun(ctx, runID)
	if err != nil {
		return err
	}
	if !terminalStoreRun(run.Status) {
		return store.ErrParserEvalActive
	}
	return c.deleteTerminal(ctx, *run)
}

func (c *Cleanup) Run(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		every = time.Hour
	}
	_, _ = c.CleanupExpired(ctx, 50)
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, _ = c.CleanupExpired(ctx, 50)
		}
	}
}

func (c *Cleanup) deleteTerminal(ctx context.Context, run store.ParserEvalRunRecord) error {
	if !terminalStoreRun(run.Status) {
		return store.ErrParserEvalActive
	}
	prefix, err := RunObjectPrefix(run.ID)
	if err != nil {
		return err
	}
	if err := c.objects.DeletePrefix(ctx, prefix); err != nil {
		return err
	}
	deleted, err := c.store.PurgeParserEvalRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if !deleted {
		return store.ErrNotFound
	}
	return nil
}

func terminalStoreRun(status string) bool {
	return status == store.ParserEvalRunSucceeded || status == store.ParserEvalRunPartial || status == store.ParserEvalRunFailed || status == store.ParserEvalRunCancelled
}
