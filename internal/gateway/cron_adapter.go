package gateway

import (
	"context"
	"time"

	"github.com/qs3c/bkcrab/internal/cron"
	"github.com/qs3c/bkcrab/internal/store"
)

// cronStoreAdapter 桥接 store.Store 到 cron.StoreInterface。cron
// 包维护自己的 StoreJob 类型以避免导入循环；我们在此将 CronJobRecord 行投影到该类型。
type cronStoreAdapter struct {
	st store.Store
}

func (a *cronStoreAdapter) GetDueCronJobs(ctx context.Context, now time.Time) ([]cron.StoreJob, error) {
	rows, err := a.st.GetDueCronJobs(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make([]cron.StoreJob, 0, len(rows))
	// 每个（去重后的）代理解析一次 agent → owner，这样同一个代理触发数十个任务时不会重复查询。
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
