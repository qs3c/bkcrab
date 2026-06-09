package store

import (
	"context"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DBStore {
	t.Helper()
	db, err := NewDBStore("sqlite", "file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

func TestCronJobSQLiteRoundTrip(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()

	// Create a cron job with a future NextRun (simulates "once" type)
	futureTime := time.Now().Add(5 * time.Minute).UTC()
	job := &CronJobRecord{
		ID:        "test-once-1",
		AgentID:   "user-test",
		Name:      "test reminder",
		Type:      "once",
		Schedule:  futureTime.Format(time.RFC3339),
		Message:   "remind me",
		Channel:   "pinclaw",
		ChatID:    "web-ui",
		Timezone:  "UTC",
		Enabled:   true,
		NextRun:   &futureTime,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.SaveCronJob(ctx, job); err != nil {
		t.Fatalf("save cron job: %v", err)
	}

	// Verify: GetDueCronJobs with a time BEFORE NextRun should return nothing
	earlyTime := time.Now().UTC()
	due, err := db.GetDueCronJobs(ctx, earlyTime)
	if err != nil {
		t.Fatalf("GetDueCronJobs (early): %v", err)
	}
	if len(due) != 0 {
		t.Errorf("expected 0 due jobs before NextRun, got %d", len(due))
	}

	// Verify: GetDueCronJobs with a time AFTER NextRun should return the job
	lateTime := futureTime.Add(time.Minute)
	due, err = db.GetDueCronJobs(ctx, lateTime)
	if err != nil {
		t.Fatalf("GetDueCronJobs (late): %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("expected 1 due job after NextRun, got %d", len(due))
	}
	if due[0].ID != "test-once-1" {
		t.Errorf("expected job ID test-once-1, got %s", due[0].ID)
	}

	// Verify: GetNextDueTime should return the job's NextRun
	nextDue, err := db.GetNextDueTime(ctx)
	if err != nil {
		t.Fatalf("GetNextDueTime: %v", err)
	}
	if nextDue.IsZero() {
		t.Fatal("GetNextDueTime returned zero time")
	}
	diff := nextDue.Sub(futureTime)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("GetNextDueTime should match NextRun. got=%v want=%v diff=%v", nextDue, futureTime, diff)
	}

	// Verify: DeleteCronJob works
	if err := db.DeleteCronJob(ctx, "test-once-1"); err != nil {
		t.Fatalf("DeleteCronJob: %v", err)
	}
	due, err = db.GetDueCronJobs(ctx, lateTime)
	if err != nil {
		t.Fatalf("GetDueCronJobs (after delete): %v", err)
	}
	if len(due) != 0 {
		t.Errorf("expected 0 jobs after delete, got %d", len(due))
	}
}

func TestCronJobSQLiteInterval(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	nextRun := now.Add(30 * time.Minute)

	job := &CronJobRecord{
		ID:        "test-interval-1",
		AgentID:   "user-test",
		Name:      "interval test",
		Type:      "interval",
		Schedule:  "30m",
		Message:   "check",
		Channel:   "pinclaw",
		ChatID:    "web-ui",
		Timezone:  "UTC",
		Enabled:   true,
		NextRun:   &nextRun,
		CreatedAt: now,
	}
	if err := db.SaveCronJob(ctx, job); err != nil {
		t.Fatalf("save: %v", err)
	}

	// UpdateCronJobRun then verify next_run changed
	newNextRun := now.Add(60 * time.Minute)
	if err := db.UpdateCronJobRun(ctx, "test-interval-1", now, newNextRun); err != nil {
		t.Fatalf("UpdateCronJobRun: %v", err)
	}

	// Job should not be due at now+45m
	due, err := db.GetDueCronJobs(ctx, now.Add(45*time.Minute))
	if err != nil {
		t.Fatalf("GetDueCronJobs: %v", err)
	}
	if len(due) != 0 {
		t.Errorf("job with next_run=now+60m should not be due at now+45m, got %d", len(due))
	}

	// But should be due at now+61m
	due, err = db.GetDueCronJobs(ctx, now.Add(61*time.Minute))
	if err != nil {
		t.Fatalf("GetDueCronJobs: %v", err)
	}
	if len(due) != 1 {
		t.Errorf("job with next_run=now+60m should be due at now+61m, got %d", len(due))
	}
}
