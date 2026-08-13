package rag

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestLegacyGenerationBackfillEmptyPartialDeletingIdempotentAndConcurrent(t *testing.T) {
	st := newRAGTestStore(t)
	ctx := context.Background()
	empty := seedLegacyGenerationKB(t, st, "kb_legacy_empty")
	partial := seedLegacyGenerationKB(t, st, "kb_legacy_partial")
	seedLegacyGenerationDocument(t, st, partial.ID, "doc_active", "DONE", 3, 7)
	seedLegacyGenerationDocument(t, st, partial.ID, "doc_active_two", "DONE", 2, 5)
	seedLegacyGenerationVersionHistory(t, st, "doc_active_two", 1, 2)
	seedLegacyGenerationDocument(t, st, partial.ID, "doc_pending", "PENDING", 0, 0)
	seedLegacyGenerationDocument(t, st, partial.ID, "doc_deleting", store.RAGDocumentStatusDeleting, 4, 11)

	const workers = 8
	start := make(chan struct{})
	errs := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			errs <- st.BackfillLegacyRAGGenerations(ctx)
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent backfill: %v", err)
		}
	}
	if err := st.BackfillLegacyRAGGenerations(ctx); err != nil {
		t.Fatalf("idempotent backfill: %v", err)
	}

	emptyGeneration, emptyDocuments, err := st.ResolveActiveRAGKBGeneration(ctx, empty.ID)
	if err != nil || emptyGeneration.CollectionKey != empty.ID || len(emptyDocuments) != 0 {
		t.Fatalf("empty generation=%+v documents=%+v err=%v", emptyGeneration, emptyDocuments, err)
	}
	partialGeneration, documents, err := st.ResolveActiveRAGKBGeneration(ctx, partial.ID)
	if err != nil {
		t.Fatal(err)
	}
	if partialGeneration.CollectionKey != partial.ID || partialGeneration.DocumentCount != 2 || len(documents) != 2 ||
		documents[0].DocID != "doc_active" || documents[0].DocVersion != 3 || documents[0].Status != store.RAGGenerationDocumentReady ||
		documents[1].DocID != "doc_active_two" || documents[1].DocVersion != 2 || documents[1].Status != store.RAGGenerationDocumentReady {
		t.Fatalf("partial generation=%+v documents=%+v", partialGeneration, documents)
	}
	versions, err := st.ListRAGDocumentVersions(ctx, "doc_active_two")
	if err != nil || len(versions) != 2 || versions[0].DocVersion != 1 || versions[1].DocVersion != 2 {
		t.Fatalf("document history changed during generation backfill: versions=%+v err=%v", versions, err)
	}

	var generationCount, policyCount int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_kb_index_generations`).Scan(&generationCount); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_ingestion_policies`).Scan(&policyCount); err != nil {
		t.Fatal(err)
	}
	if generationCount != 2 || policyCount != 2 {
		t.Fatalf("idempotent counts generations=%d policies=%d", generationCount, policyCount)
	}
	var policyJSON, fingerprint, policyStatus string
	if err := st.DB().QueryRowContext(ctx, `SELECT policy_json,fingerprint,status FROM rag_ingestion_policies WHERE version=?`, partialGeneration.PolicyVersion).Scan(&policyJSON, &fingerprint, &policyStatus); err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err := json.Unmarshal([]byte(policyJSON), &policy); err != nil {
		t.Fatal(err)
	}
	if policy["source"] != "legacy-backfill" || len(fingerprint) != 64 || policyStatus != store.RAGPolicyRetired {
		t.Fatalf("legacy policy snapshot=%s fingerprint=%q status=%q", policyJSON, fingerprint, policyStatus)
	}
}

func TestGenerationResolverShadowValidationAuthorityAndLegacyFallback(t *testing.T) {
	st := newRAGTestStore(t)
	ctx := context.Background()
	kb := seedLegacyGenerationKB(t, st, "kb_resolver")
	seedLegacyGenerationDocument(t, st, kb.ID, "doc", "DONE", 2, 5)
	if err := st.BackfillLegacyRAGGenerations(ctx); err != nil {
		t.Fatal(err)
	}
	generation, _, err := st.ResolveActiveRAGKBGeneration(ctx, kb.ID)
	if err != nil {
		t.Fatal(err)
	}

	var alerts atomic.Int32
	report := func(context.Context, string, string) { alerts.Add(1) }
	shadow := NewGenerationResolver(st, GenerationResolutionShadow, report)
	key, err := shadow.ResolveCollection(ctx, kb.ID)
	if err != nil || key != vector.CollectionKey(kb.ID) || alerts.Load() != 0 {
		t.Fatalf("valid shadow key=%q alerts=%d err=%v", key, alerts.Load(), err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_kb_generation_documents SET doc_version=99 WHERE generation_id=? AND doc_id='doc'`, generation.ID); err != nil {
		t.Fatal(err)
	}
	key, err = shadow.ResolveCollection(ctx, kb.ID)
	if err != nil || key != vector.CollectionKey(kb.ID) || alerts.Load() != 1 {
		t.Fatalf("mismatched shadow key=%q alerts=%d err=%v", key, alerts.Load(), err)
	}
	authoritative := NewGenerationResolver(st, GenerationResolutionAuthoritative, report)
	key, err = authoritative.ResolveSearchCollection(ctx, kb.ID, map[string]int64{"doc": 2})
	if err != nil || key != vector.CollectionKey(kb.ID) || alerts.Load() != 2 {
		t.Fatalf("mismatched authoritative search key=%q alerts=%d err=%v", key, alerts.Load(), err)
	}

	generatedKey, err := vector.GenerationCollectionKey(kb.ID, "generation_02")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_kb_generation_documents SET doc_version=2 WHERE generation_id=? AND doc_id='doc'`, generation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_kb_index_generations SET collection_key=? WHERE id=?`, string(generatedKey), generation.ID); err != nil {
		t.Fatal(err)
	}
	key, err = authoritative.ResolveCollection(ctx, kb.ID)
	if err != nil || key != generatedKey {
		t.Fatalf("authoritative key=%q want=%q err=%v", key, generatedKey, err)
	}

	unmigrated := seedLegacyGenerationKB(t, st, "kb_unmigrated")
	key, err = authoritative.ResolveCollection(ctx, unmigrated.ID)
	if err != nil || key != vector.CollectionKey(unmigrated.ID) {
		t.Fatalf("legacy fallback key=%q err=%v", key, err)
	}
}

func seedLegacyGenerationKB(t *testing.T, st *store.DBStore, id string) *store.RAGKBRecord {
	t.Helper()
	kb := &store.RAGKBRecord{
		ID: id, UserID: "u1", Name: id, EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 4,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: store.RAGParseModeStandard, Status: "active",
	}
	if err := st.CreateRAGKB(context.Background(), kb); err != nil {
		t.Fatal(err)
	}
	return kb
}

func seedLegacyGenerationDocument(t *testing.T, st *store.DBStore, kbID, docID, status string, activeVersion int64, chunkCount int) {
	t.Helper()
	doc := &store.RAGDocumentRecord{
		ID: docID, KBID: kbID, FileName: docID + ".md", FileType: "md", ObjectKey: fmt.Sprintf("rag/u1/%s/%s.md", kbID, docID),
		Status: status, Version: max(activeVersion, 1), ActiveVersion: activeVersion, ChunkCount: chunkCount, IndexFormatVersion: 1,
	}
	if err := st.CreateRAGDocument(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
}

func seedLegacyGenerationVersionHistory(t *testing.T, st *store.DBStore, docID string, versions ...int64) {
	t.Helper()
	for _, version := range versions {
		record := &store.RAGDocumentVersionRecord{
			DocID: docID, DocVersion: version,
			SourceSHA256: strings.Repeat("a", 64), ParseMode: store.RAGParseModeStandard,
			ChunkSize: 512, ChunkOverlap: 64, ParserVersion: "legacy-parser", SplitterVersion: "legacy-splitter",
			ParseFingerprint: strings.Repeat("b", 64), IndexFingerprint: strings.Repeat("c", 64),
			EmbeddingProvider: "system", EmbeddingModel: "embed-v1", EmbeddingDimensions: 4,
			EmbeddingContractFingerprint: strings.Repeat("d", 64),
			MaxDocumentAIRequests:        20, MaxDocumentAITokens: 10_000, MaxDocumentAICostMicroUSD: 50_000,
		}
		if err := st.CreateRAGDocumentVersion(context.Background(), record); err != nil {
			t.Fatalf("create historical version %s/%d: %v", docID, version, err)
		}
		if _, err := st.DB().ExecContext(context.Background(), `UPDATE rag_document_versions SET status='DONE' WHERE doc_id=? AND doc_version=?`, docID, version); err != nil {
			t.Fatalf("complete historical version %s/%d: %v", docID, version, err)
		}
	}
}
