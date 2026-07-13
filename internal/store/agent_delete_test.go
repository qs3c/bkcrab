package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestDeleteAgentRemovesSkillUsage(t *testing.T) {
	db, err := NewDBStore("sqlite", "file:delete-agent-skills?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.SaveAgent(ctx, &AgentRecord{ID: "agent-delete", UserID: "owner", Name: "Delete", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSkillUsage(ctx, "agent-delete", "learned", HashSkillContent("content"), true); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteAgent(ctx, "agent-delete"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListSkillUsage(ctx, "agent-delete")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("skill_usage survived agent deletion: %+v", rows)
	}
	var lifecycleRows int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_skill_lifecycle WHERE agent_id='agent-delete'`).Scan(&lifecycleRows); err != nil {
		t.Fatal(err)
	}
	if lifecycleRows != 0 {
		t.Fatalf("agent_skill_lifecycle survived agent deletion: %d", lifecycleRows)
	}
}

func TestConcurrentSaveCannotResurrectDeletedAgent(t *testing.T) {
	db := newTestSQLite(t)
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("race-agent-%d", i)
		agent := &AgentRecord{ID: id, UserID: "owner", Name: "before"}
		if err := db.SaveAgent(ctx, agent); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			copy := *agent
			copy.Name = "concurrent-save"
			_ = db.SaveAgent(ctx, &copy)
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = db.DeleteAgent(ctx, id)
		}()
		close(start)
		wg.Wait()
		deleting, err := db.IsAgentDeleting(ctx, id)
		if err != nil || !deleting {
			t.Fatalf("iteration %d tombstone=(%v,%v)", i, deleting, err)
		}
		if got, err := db.GetAgent(ctx, id); err == nil || got != nil {
			t.Fatalf("iteration %d resurrected agent: got=%+v err=%v", i, got, err)
		}
	}
}

func TestDeleteUserRemovesOwnedAgentSkillUsage(t *testing.T) {
	db, err := NewDBStore("sqlite", "file:delete-user-skills?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.CreateUser(ctx, &UserRecord{
		ID: "owner-delete", Username: "owner-delete", Email: "owner@example.test",
		PasswordHash: "hash", Role: "user", Status: "active", AgentQuota: -1,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveAgent(ctx, &AgentRecord{ID: "owned-agent", UserID: "owner-delete", Name: "Owned", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSkillUsage(ctx, "owned-agent", "learned", HashSkillContent("content"), true); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteUser(ctx, "owner-delete"); err != nil {
		t.Fatal(err)
	}
	rows, err := db.ListSkillUsage(ctx, "owned-agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("skill_usage survived owner deletion: %+v", rows)
	}
	var lifecycleRows int
	if err := db.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agent_skill_lifecycle WHERE agent_id='owned-agent'`).Scan(&lifecycleRows); err != nil {
		t.Fatal(err)
	}
	if lifecycleRows != 0 {
		t.Fatalf("agent_skill_lifecycle survived owner deletion: %d", lifecycleRows)
	}
}

func TestAgentDeletionMarkerIsDurableAndBlocksIDReuse(t *testing.T) {
	db, err := NewDBStore("sqlite", "file:agent-deletion-marker?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	agent := &AgentRecord{ID: "never-reuse", UserID: "owner", Name: "Original", CreatedAt: now, UpdatedAt: now}
	if err := db.SaveAgent(ctx, agent); err != nil {
		t.Fatal(err)
	}
	if err := db.MarkAgentDeleting(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	deleting, err := db.IsAgentDeleting(ctx, agent.ID)
	if err != nil || !deleting {
		t.Fatalf("deletion marker = (%v, %v), want true", deleting, err)
	}
	agent.Name = "Recreated"
	if err := db.SaveAgent(ctx, agent); err == nil {
		t.Fatal("SaveAgent reused a tombstoned agent id")
	}
}
