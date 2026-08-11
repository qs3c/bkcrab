package rag

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	rageval "github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/rag/objects"
	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

func publishIngestionRevision(t *testing.T, st *store.DBStore, expected int64, policy config.RAGIngestionPolicyData) {
	t.Helper()
	raw, _ := json.Marshal(policy)
	fingerprint, _ := rageval.Fingerprint(policy)
	record := &store.RAGPolicyRecord{Kind: store.RAGPolicyIngestion, Version: policy.Version, PolicyJSON: string(raw), Fingerprint: fingerprint, CreatedBy: "admin"}
	if err := st.CreateRAGPolicy(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ActivateRAGPolicy(context.Background(), store.RAGPolicyIngestion, expected, policy.Version, "admin", "", "test", store.RAGPolicyAuditPublish); err != nil || !ok {
		t.Fatalf("activate ingestion v%d=%v err=%v", policy.Version, ok, err)
	}
}

func TestIngestionPolicyPinsNewKBAndKeepsOldKBUploadsOnPinnedRevision(t *testing.T) {
	embedding := newEmbeddingServer(t)
	cfg := config.RAGCfg{Milvus: config.MilvusCfg{Address: "fake"}, Embedding: config.RAGEmbeddingCfg{Endpoint: embedding.URL, Model: "embed-test", Dims: 4}}
	st := newRAGTestStore(t)
	vec := vector.NewFake()
	service := New(Deps{Store: st, Vector: vec, Objects: objects.NewLocalFS(t.TempDir()), Cfg: cfg})

	v1 := DefaultIngestionPolicy(cfg)
	v1.ChunkSize, v1.ChunkOverlap = 256, 32
	publishIngestionRevision(t, st, 0, v1)
	oldKB, err := service.CreateKB(context.Background(), "u1", "old", "", 999, 111)
	if err != nil {
		t.Fatal(err)
	}
	if !oldKB.PinnedPolicyVersion.Valid || oldKB.PinnedPolicyVersion.Int64 != 1 || !oldKB.ActiveGenerationID.Valid {
		t.Fatalf("old KB was not pinned atomically: %+v", oldKB)
	}
	oldGeneration, _, err := st.ResolveActiveRAGKBGeneration(context.Background(), oldKB.ID)
	if err != nil || oldGeneration.PolicyVersion != 1 || oldGeneration.EmbeddingDims != 4 || !vec.HasCollection(vector.CollectionKey(oldGeneration.CollectionKey)) {
		t.Fatalf("initial generation=%+v err=%v", oldGeneration, err)
	}

	v2 := v1
	v2.Version, v2.ChunkSize, v2.ChunkOverlap = 2, 768, 96
	v2.DocumentAI.VisionPromptVersion = "vision-v2"
	publishIngestionRevision(t, st, 1, v2)
	newKB, err := service.CreateKB(context.Background(), "u1", "new", "", 128, 16)
	if err != nil {
		t.Fatal(err)
	}
	if newKB.PinnedPolicyVersion.Int64 != 2 || newKB.ChunkSize != 768 || oldKB.ChunkSize != 256 {
		t.Fatalf("old=%+v new=%+v", oldKB, newKB)
	}

	payload := "pinned policy upload"
	doc, err := service.UploadDocument(context.Background(), "u1", oldKB.ID, "old.txt", strings.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.GetRAGDocumentVersion(context.Background(), doc.ID, doc.Version)
	if err != nil || version.ChunkSize != 256 || version.ChunkOverlap != 32 || version.VisionPromptVersion != v1.DocumentAI.VisionPromptVersion {
		t.Fatalf("old KB upload inherited latest policy: version=%+v err=%v", version, err)
	}
	status, err := service.GetKBIngestionPolicyStatus(context.Background(), "u1", oldKB.ID)
	if err != nil || !status.Drift || status.PinnedVersion != 1 || status.LatestVersion != 2 || status.FullCollectionRebuild {
		t.Fatalf("drift status=%+v err=%v", status, err)
	}
	for _, diff := range status.Differences {
		if strings.Contains(strings.ToLower(diff.Field), "endpoint") || strings.Contains(strings.ToLower(diff.Field), "key") {
			t.Fatalf("secret-bearing field leaked into diff: %+v", diff)
		}
	}
}

func TestIngestionPolicyDiffMarksEmbeddingChangeAsFullCollectionRebuild(t *testing.T) {
	from := config.RAGIngestionPolicyData{Embedding: config.RAGPolicyEmbeddingData{ContractFingerprint: "a", Model: "m1", Dims: 4}}
	to := from
	to.Embedding = config.RAGPolicyEmbeddingData{ContractFingerprint: "b", Model: "m2", Dims: 8}
	diffs := ingestionPolicyDifferences(from, to)
	if len(diffs) != 3 {
		t.Fatalf("embedding diffs=%+v", diffs)
	}
}
