package gateway

import (
	"context"
	"time"

	"github.com/qs3c/bkclaw/internal/cron"
	"github.com/qs3c/bkclaw/internal/store"
)

// cronStoreAdapter bridges store.Store to cron.StoreInterface. The cron
// package keeps its own StoreJob type to avoid an import cycle; we project
// CronJobRecord rows down to that type here.
type cronStoreAdapter struct {
	st store.Store
}

func (a *cronStoreAdapter) GetDueCronJobs(ctx context.Context, now time.Time) ([]cron.StoreJob, error) {
	rows, err := a.st.GetDueCronJobs(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make([]cron.StoreJob, 0, len(rows))
	// Resolve agent → owner once per (de-duplicated) agent so a tick
	// firing dozens of jobs against the same agent doesn't re-query.
	ownerByAgent := map[string]string{}
	for _, r := range rows {
		owner, ok := ownerByAgent[r.AgentID]
		if !ok {
			if ag, err := a.st.GetAgent(ctx, r.AgentID); err == nil && ag != nil {
				owner = ag.UserID
			}
			ownerByAgent[r.AgentID] = owner
		}
		out = append(out, cron.StoreJob{
			ID:          r.ID,
			AgentID:     r.AgentID,
			OwnerUserID: owner,
			Name:        r.Name,
			Type:        r.Type,
			Schedule:    r.Schedule,
			Message:     r.Message,
			Channel:     r.Channel,
			ChatID:      r.ChatID,
			AccountID:   r.AccountID,
			Timezone:    r.Timezone,
		})
	}
	return out, nil
}

func (a *cronStoreAdapter) LockCronJob(ctx context.Context, jobID, instanceID string) (bool, error) {
	return a.st.LockCronJob(ctx, jobID, instanceID)
}

func (a *cronStoreAdapter) UpdateCronJobRun(ctx context.Context, jobID string, lastRun, nextRun time.Time) error {
	return a.st.UpdateCronJobRun(ctx, jobID, lastRun, nextRun)
}

func (a *cronStoreAdapter) IncrementCronJobFailure(ctx context.Context, jobID string) (int, error) {
	return a.st.IncrementCronJobFailure(ctx, jobID)
}

func (a *cronStoreAdapter) DeleteCronJob(ctx context.Context, jobID string) error {
	return a.st.DeleteCronJob(ctx, jobID)
}

func (a *cronStoreAdapter) GetNextDueTime(ctx context.Context) (time.Time, error) {
	return a.st.GetNextDueTime(ctx)
}
