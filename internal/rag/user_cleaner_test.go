package rag

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/users"
)

func TestRAGUserCleanupRetainsDeletingUserUntilExternalCleanupRetrySucceeds(t *testing.T) {
	ctx := context.Background()
	st := newRAGTestStore(t)
	embedding := newEmbeddingServer(t)
	vec := &failOnceDeleteVector{Fake: vector.NewFake(), failKBDelete: true}
	service := New(Deps{
		Store: st, Vector: vec, Objects: objects.NewLocalFS(t.TempDir()),
		Cfg: config.RAGCfg{
			Milvus:    config.MilvusCfg{Address: "fake"},
			Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4},
		},
	})
	kb, err := service.CreateKB(ctx, "u1", "user cleanup", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := "user-owned RAG object"
	if _, err := service.UploadDocument(ctx, "u1", kb.ID, "owned.txt", strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatal(err)
	}
	accounts, err := users.NewAccounts(st)
	if err != nil {
		t.Fatal(err)
	}
	accounts.SetRAGUserCleaner(service)

	if err := accounts.Delete(ctx, "u1"); !errors.Is(err, users.ErrRAGUserCleanupIncomplete) {
		t.Fatalf("first user cleanup error=%v", err)
	}
	user, err := st.GetUser(ctx, "u1")
	if err != nil || user.Status != users.StatusDeleting {
		t.Fatalf("cleanup failure did not retain user tombstone: user=%+v err=%v", user, err)
	}
	markedKB, err := st.GetRAGKB(ctx, kb.ID)
	if err != nil || markedKB.Status != store.RAGKBStatusDeleting {
		t.Fatalf("cleanup failure did not retain KB tombstone: kb=%+v err=%v", markedKB, err)
	}

	if err := accounts.Delete(ctx, "u1"); err != nil {
		t.Fatalf("user cleanup retry failed: %v", err)
	}
	if _, err := st.GetUser(ctx, "u1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user row remained after complete cleanup: %v", err)
	}
	if _, err := st.GetRAGKB(ctx, kb.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RAG KB remained after complete cleanup: %v", err)
	}
	if vec.HasCollection(kb.ID) {
		t.Fatal("vector collection remained after complete user cleanup")
	}
}
