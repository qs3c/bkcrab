package rag

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestRealMilvusGenerationDimensionsAtomicSwitchAndRollback(t *testing.T) {
	address := os.Getenv("RAG_TEST_MILVUS_ADDR")
	if address == "" {
		t.Skip("RAG_TEST_MILVUS_ADDR is required for the real Milvus release gate")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	milvus, err := vector.NewMilvus(ctx, address, os.Getenv("RAG_TEST_MILVUS_USER"), os.Getenv("RAG_TEST_MILVUS_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = milvus.Close(closeCtx)
	})

	st := newRAGTestStore(t)
	suffix := fmt.Sprint(time.Now().UnixNano())
	kb := &store.RAGKBRecord{ID: "kb_real_gate_" + suffix, UserID: "u1", Name: "real gate", EmbedProvider: "system", EmbedModel: "embed-v2", EmbedDims: 4, ChunkSize: 512, ChunkOverlap: 64, ParseMode: store.RAGParseModeStandard, Status: "active"}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatal(err)
	}

	type physicalGeneration struct {
		id   string
		key  vector.CollectionKey
		dims int
		text string
	}
	generations := []physicalGeneration{{id: "v2_" + suffix, dims: 4, text: "visible-v2"}, {id: "v3_" + suffix, dims: 8, text: "visible-v3"}}
	for index := range generations {
		generation := &generations[index]
		generation.key, err = vector.GenerationCollectionKey(kb.ID, generation.id)
		if err != nil {
			t.Fatal(err)
		}
		if err := milvus.EnsureCollection(ctx, generation.key, generation.dims); err != nil {
			t.Fatal(err)
		}
		key := generation.key
		t.Cleanup(func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cleanupCancel()
			_ = milvus.DropCollection(cleanupCtx, key)
		})
		values := make([]float32, generation.dims)
		values[0] = 1
		if err := milvus.UpsertChunks(ctx, generation.key, []vector.ChunkData{{DocID: "doc", DocVersion: int64(index + 2), Index: 0, Content: generation.text, Vector: values}}); err != nil {
			t.Fatal(err)
		}
	}

	activate := func(g physicalGeneration, policyVersion int64, expectedActive string, whileBuilding func()) {
		record := &store.RAGKBGenerationRecord{ID: g.id, KBID: kb.ID, PolicyVersion: policyVersion, CollectionKey: string(g.key), EmbeddingModel: "embed-v" + fmt.Sprint(policyVersion), EmbeddingDims: g.dims, CreatedBy: "phase-h"}
		if err := st.CreateRAGKBGeneration(ctx, record, nil); err != nil {
			t.Fatal(err)
		}
		task := &store.RAGPolicySyncTaskRecord{ID: "sync_" + g.id, KBID: kb.ID, TargetGenerationID: g.id, TargetPolicyVersion: policyVersion, RequestedBy: "phase-h"}
		if err := st.CreateRAGPolicySyncTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		fence, ok, err := st.ClaimRAGPolicySyncTask(ctx, task.ID, "phase-h", time.Minute)
		if err != nil || !ok {
			t.Fatalf("claim generation %s: ok=%v err=%v", g.id, ok, err)
		}
		if whileBuilding != nil {
			whileBuilding()
		}
		if ok, err = st.MarkRAGKBGenerationReady(ctx, *fence, 0, 1); err != nil || !ok {
			t.Fatalf("ready generation %s: ok=%v err=%v", g.id, ok, err)
		}
		if ok, err = st.ActivateRAGKBGeneration(ctx, *fence, expectedActive, "phase-h", "real Milvus gate", time.Hour); err != nil || !ok {
			t.Fatalf("activate generation %s: ok=%v err=%v", g.id, ok, err)
		}
	}

	activate(generations[0], 2, "", nil)
	assertRealGenerationVisible(t, ctx, st, milvus, kb.ID, generations[0])
	activate(generations[1], 3, generations[0].id, func() {
		assertRealGenerationVisible(t, ctx, st, milvus, kb.ID, generations[0])
	})
	assertRealGenerationVisible(t, ctx, st, milvus, kb.ID, generations[1])
	if ok, err := st.RollbackRAGKBGeneration(ctx, kb.ID, generations[0].id, generations[1].id, "phase-h", "real Milvus rollback", time.Hour); err != nil || !ok {
		t.Fatalf("rollback: ok=%v err=%v", ok, err)
	}
	assertRealGenerationVisible(t, ctx, st, milvus, kb.ID, generations[0])
}

func assertRealGenerationVisible(t *testing.T, ctx context.Context, st *store.DBStore, milvus vector.Store, kbID string, want struct {
	id   string
	key  vector.CollectionKey
	dims int
	text string
}) {
	t.Helper()
	active, _, err := st.ResolveActiveRAGKBGeneration(ctx, kbID)
	if err != nil || active.ID != want.id || active.CollectionKey != string(want.key) {
		t.Fatalf("active generation=%+v err=%v, want %s", active, err, want.id)
	}
	query := make([]float32, want.dims)
	query[0] = 1
	hits, err := milvus.HybridSearch(ctx, want.key, vector.SearchQuery{Dense: [][]float32{query}}, 10)
	if err != nil || len(hits) != 1 || hits[0].Content != want.text {
		t.Fatalf("visible hits=%+v err=%v, want only %q", hits, err, want.text)
	}
}
