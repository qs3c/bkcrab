package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestRAGChatTurnPersistenceAndKBCleanup(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	ctx := context.Background()
	base := time.Date(2026, 7, 16, 9, 0, 0, 0, time.UTC)

	kb := &RAGKBRecord{
		ID: "kb_chat", UserID: "u1", Name: "手册", EmbedModel: "embed",
		EmbedDims: 4, ChunkSize: 512, ChunkOverlap: 64, Status: "active",
		CreatedAt: base,
	}
	if err := db.CreateRAGKB(ctx, kb); err != nil {
		t.Fatal(err)
	}

	turns := []RAGChatTurnRecord{
		{ID: "turn_1", UserID: "u1", KBID: kb.ID, SessionID: "session_a", Title: "默认端口", Question: "默认端口是什么？", Answer: "8080", Sources: json.RawMessage(`[{"docName":"a.md"}]`), CreatedAt: base},
		{ID: "turn_2", UserID: "u1", KBID: kb.ID, SessionID: "session_a", Title: "默认端口", Question: "Linux 也一样吗？", Answer: "一样", Sources: json.RawMessage(`[]`), CreatedAt: base.Add(time.Minute)},
		{ID: "turn_3", UserID: "u1", KBID: kb.ID, SessionID: "session_b", Title: "安装要求", Question: "如何安装？", Answer: "按文档安装", Sources: json.RawMessage(`[]`), CreatedAt: base.Add(2 * time.Minute)},
		{ID: "turn_4", UserID: "u2", KBID: kb.ID, SessionID: "session_private", Title: "其他用户", Question: "问题", Answer: "回答", Sources: json.RawMessage(`[]`), CreatedAt: base.Add(3 * time.Minute)},
	}
	for index := range turns {
		if err := db.AppendRAGChatTurn(ctx, &turns[index]); err != nil {
			t.Fatalf("append turn %d: %v", index, err)
		}
	}

	gotTurns, err := db.ListRAGChatTurns(ctx, "u1", kb.ID, "session_a")
	if err != nil {
		t.Fatal(err)
	}
	if len(gotTurns) != 2 || gotTurns[0].Question != turns[0].Question || string(gotTurns[0].Sources) != string(turns[0].Sources) {
		t.Fatalf("session_a turns = %+v", gotTurns)
	}
	privateTurns, err := db.ListRAGChatTurns(ctx, "u1", kb.ID, "session_private")
	if err != nil || len(privateTurns) != 0 {
		t.Fatalf("cross-user turns = %+v, err=%v", privateTurns, err)
	}

	sessions, err := db.ListRAGChatSessions(ctx, "u1", kb.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 || sessions[0].ID != "session_b" || sessions[1].ID != "session_a" || sessions[1].TurnCount != 2 {
		t.Fatalf("sessions = %+v", sessions)
	}

	if err := db.DeleteRAGKB(ctx, kb.ID); err != nil {
		t.Fatal(err)
	}
	remaining, err := db.ListRAGChatSessions(ctx, "u1", kb.ID, 10)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("sessions after KB delete = %+v, err=%v", remaining, err)
	}
}
