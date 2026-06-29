package store_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/usage"
)

func TestMySQLStoreIntegration(t *testing.T) {
	dsn := os.Getenv("BKCRAB_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("BKCRAB_TEST_MYSQL_DSN is not set")
	}

	ctx := context.Background()
	st, err := store.NewDBStore("mysql", dsn)
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}

	suffix := time.Now().UTC().Format("20060102150405.000000000")
	userID := "u_mysql_" + suffix
	agentID := "a_mysql_" + suffix
	sessionKey := "s_mysql_" + suffix

	if err := st.CreateUser(ctx, &store.UserRecord{
		ID:          userID,
		Username:    "mysql-" + suffix,
		Email:       "mysql-" + suffix + "@example.com",
		DisplayName: "MySQL Test",
		Role:        "user",
		Status:      "active",
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}

	agent := &store.AgentRecord{ID: agentID, UserID: userID, Name: "first"}
	if err := st.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("save agent: %v", err)
	}
	agent.Name = "updated"
	if err := st.SaveAgent(ctx, agent); err != nil {
		t.Fatalf("update agent: %v", err)
	}
	gotAgent, err := st.GetAgent(ctx, agentID)
	if err != nil || gotAgent.Name != "updated" {
		t.Fatalf("get updated agent: agent=%#v err=%v", gotAgent, err)
	}

	session := &store.SessionRecord{Channel: "web", ChatID: sessionKey}
	if err := st.SaveSession(ctx, userID, agentID, sessionKey, session); err != nil {
		t.Fatalf("save session: %v", err)
	}
	if err := st.SaveSession(ctx, userID, agentID, sessionKey, session); err != nil {
		t.Fatalf("update session: %v", err)
	}
	if err := st.AppendSessionMessage(ctx, userID, agentID, sessionKey, store.SessionMessage{
		Role:    "user",
		Content: "hello mysql",
	}); err != nil {
		t.Fatalf("append session message: %v", err)
	}
	messages, err := st.ListSessionMessages(ctx, userID, agentID, sessionKey)
	if err != nil || len(messages) != 1 {
		t.Fatalf("list session messages: messages=%#v err=%v", messages, err)
	}
	seq, err := st.AppendSessionEvent(ctx, userID, agentID, sessionKey, "content", []byte(`{"text":"ok"}`))
	if err != nil || seq != 0 {
		t.Fatalf("append session event: seq=%d err=%v", seq, err)
	}

	cfg := &store.ConfigRecord{
		Kind:    store.KindSetting,
		UserID:  userID,
		AgentID: agentID,
		Name:    "mysql.test",
		Enabled: true,
		Data:    map[string]interface{}{"value": "first"},
	}
	if err := st.SaveConfig(ctx, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	cfg.Data["value"] = "updated"
	if err := st.SaveConfig(ctx, cfg); err != nil {
		t.Fatalf("update config: %v", err)
	}

	if err := st.SaveAgentFile(ctx, agentID, userID, "MYSQL.md", []byte("first")); err != nil {
		t.Fatalf("save agent file: %v", err)
	}
	if err := st.SaveAgentFile(ctx, agentID, userID, "MYSQL.md", []byte("updated")); err != nil {
		t.Fatalf("update agent file: %v", err)
	}

	now := time.Now().UTC()
	next := now.Add(time.Hour)
	job := &store.CronJobRecord{
		ID:        "cron_mysql_" + suffix,
		UserID:    userID,
		AgentID:   agentID,
		Name:      "mysql",
		Type:      "cron",
		Schedule:  "0 * * * *",
		Message:   "test",
		Channel:   "web",
		ChatID:    sessionKey,
		Timezone:  "UTC",
		Enabled:   true,
		NextRun:   &next,
		CreatedAt: now,
	}
	if err := st.SaveCronJob(ctx, job); err != nil {
		t.Fatalf("save cron: %v", err)
	}
	if err := st.SaveCronJob(ctx, job); err != nil {
		t.Fatalf("update cron: %v", err)
	}
	if failureCount, err := st.IncrementCronJobFailure(ctx, job.ID); err != nil || failureCount != 1 {
		t.Fatalf("increment cron failure: count=%d err=%v", failureCount, err)
	}
	if due, err := st.GetNextDueTime(ctx); err != nil || due.IsZero() {
		t.Fatalf("get next due time: due=%v err=%v", due, err)
	}

	project := &store.ProjectRecord{
		ID:          "p_mysql_" + suffix,
		UserID:      userID,
		AgentID:     agentID,
		Name:        "first",
		Description: "test",
	}
	if err := st.SaveProject(ctx, project); err != nil {
		t.Fatalf("save project: %v", err)
	}
	project.Name = "updated"
	if err := st.SaveProject(ctx, project); err != nil {
		t.Fatalf("update project: %v", err)
	}

	ok, err := st.AcquireChannelLease(ctx, "mysql-test", suffix, "holder-1", time.Minute)
	if err != nil || !ok {
		t.Fatalf("acquire first lease: ok=%v err=%v", ok, err)
	}
	ok, err = st.AcquireChannelLease(ctx, "mysql-test", suffix, "holder-2", time.Minute)
	if err != nil || ok {
		t.Fatalf("reject competing lease: ok=%v err=%v", ok, err)
	}
	if err := st.ReleaseChannelLease(ctx, "mysql-test", suffix, "holder-1"); err != nil {
		t.Fatalf("release lease: %v", err)
	}

	goal := &store.GoalRecord{
		ID:          "goal_mysql_" + suffix,
		AgentID:     agentID,
		SessionKey:  sessionKey,
		OwnerUserID: userID,
		Objective:   "verify MySQL",
	}
	if err := st.CreateGoal(ctx, goal); err != nil {
		t.Fatalf("create goal: %v", err)
	}
	goal.ID += "_duplicate"
	if err := st.CreateGoal(ctx, goal); !errors.Is(err, store.ErrGoalAlreadyExists) {
		t.Fatalf("duplicate goal error = %v", err)
	}

	meter := usage.NewSQLMeter(st.DB(), st.Dialect())
	if err := meter.RecordTokens(ctx, userID, agentID, sessionKey, "mysql", "test", usage.Tokens{Input: 2, Output: 3}); err != nil {
		t.Fatalf("record usage 1: %v", err)
	}
	if err := meter.RecordTokens(ctx, userID, agentID, sessionKey, "mysql", "test", usage.Tokens{Input: 5, Output: 7}); err != nil {
		t.Fatalf("record usage 2: %v", err)
	}
	totals, err := meter.Totals(ctx, usage.Range{
		Since: now.AddDate(0, 0, -1),
		Until: now.AddDate(0, 0, 1),
	})
	if err != nil {
		t.Fatalf("usage totals: %v", err)
	}
	if totals.Input < 7 || totals.Output < 10 {
		t.Fatalf("usage totals not accumulated: %#v", totals)
	}
}
