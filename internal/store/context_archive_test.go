package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestContextArchiveSQLiteRoundTripAndSessionCleanup(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	rec := &ContextArchiveRecord{
		ID:            "ctxar_roundtrip",
		UserID:        "user-a",
		AgentID:       "agent-a",
		SessionKey:    "session-a",
		ToolCallID:    "call-a",
		ToolName:      "exec",
		Content:       "original result",
		ContentBytes:  len("original result"),
		ContentSHA256: "sha-test",
		CreatedAt:     now,
	}
	if err := db.SaveContextArchive(ctx, rec); err != nil {
		t.Fatalf("save context archive: %v", err)
	}

	got, err := db.GetContextArchive(ctx, "agent-a", "session-a", "ctxar_roundtrip")
	if err != nil {
		t.Fatalf("get context archive: %v", err)
	}
	if got.UserID != rec.UserID || got.ToolCallID != rec.ToolCallID || got.ToolName != rec.ToolName || got.Content != rec.Content {
		t.Fatalf("archive record mismatch: got %+v want %+v", got, rec)
	}
	if got.ContentBytes != rec.ContentBytes || got.ContentSHA256 != rec.ContentSHA256 {
		t.Fatalf("archive size/hash mismatch: got %+v want %+v", got, rec)
	}

	if err := db.DeleteSession(ctx, "user-a", "agent-a", "session-a"); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	_, err = db.GetContextArchive(ctx, "agent-a", "session-a", "ctxar_roundtrip")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected archive to be deleted with session, got %v", err)
	}
}
