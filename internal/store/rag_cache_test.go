package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testRAGCacheRecord(
	doc *RAGDocumentRecord,
	kind, key, fingerprintKind, fingerprint string,
) RAGCacheObjectRecord {
	directory, _ := ragCacheDirectory(kind)
	return RAGCacheObjectRecord{
		DocID: doc.ID, CacheKind: kind, CacheKey: key,
		ObjectKey: fmt.Sprintf("rag/u_claim/%s/%s/cache/%s/%s.json",
			doc.KBID, doc.ID, directory, key),
		FingerprintKind: fingerprintKind, Fingerprint: fingerprint,
	}
}

func TestRAGCacheCatalogProtectsActiveAndSharedObjectsAndFencesCleanup(t *testing.T) {
	ctx := context.Background()
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_cache_catalog", 3)
	claim := activateRAGLifecycleVersion(t, st, "cache-index", 0)
	activeParse := claim.Version.ParseFingerprint
	activeIndex := claim.Version.IndexFingerprint
	oldParse := strings.Repeat("1", 64)
	expiredParse := strings.Repeat("2", 64)
	olderIndex := strings.Repeat("3", 64)
	newerIndex := strings.Repeat("4", 64)

	shared := testRAGCacheRecord(doc, RAGCacheKindPage, strings.Repeat("a", 64),
		RAGCacheFingerprintParse, activeParse)
	for _, record := range []RAGCacheObjectRecord{
		shared,
		testRAGCacheRecord(doc, RAGCacheKindPage, shared.CacheKey,
			RAGCacheFingerprintParse, oldParse),
		testRAGCacheRecord(doc, RAGCacheKindImage, strings.Repeat("b", 64),
			RAGCacheFingerprintParse, expiredParse),
		testRAGCacheRecord(doc, RAGCacheKindEnrich, strings.Repeat("c", 64),
			RAGCacheFingerprintIndex, olderIndex),
		testRAGCacheRecord(doc, RAGCacheKindEnrich, strings.Repeat("d", 64),
			RAGCacheFingerprintIndex, newerIndex),
		testRAGCacheRecord(doc, RAGCacheKindEnrich, strings.Repeat("e", 64),
			RAGCacheFingerprintIndex, activeIndex),
	} {
		if err := st.RegisterRAGCacheObject(ctx, record); err != nil {
			t.Fatalf("register %+v: %v", record, err)
		}
	}

	now := time.Now().UTC()
	timestamps := map[string]time.Time{
		expiredParse: now.Add(-2 * time.Hour),
		olderIndex:   now.Add(-10 * time.Minute),
		newerIndex:   now.Add(-5 * time.Minute),
		oldParse:     now.Add(-3 * time.Hour),
	}
	for fingerprint, updatedAt := range timestamps {
		if _, err := st.DB().ExecContext(ctx, `UPDATE rag_cache_object_fingerprints
			SET updated_at=? WHERE doc_id=? AND fingerprint=?`, updatedAt, doc.ID, fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_cache_objects SET updated_at=?
		WHERE doc_id=? AND cache_key=?`, now.Add(-2*time.Hour), doc.ID, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}

	docs, err := st.ListRAGCacheCatalogDocuments(ctx, "", 10)
	if err != nil || len(docs) != 1 || docs[0] != doc.ID {
		t.Fatalf("cache catalog documents=%v err=%v", docs, err)
	}
	fence, err := st.ClaimRAGDocumentMaintenance(ctx, doc.ID, "cache-cleaner", time.Minute)
	if err != nil || fence == nil {
		t.Fatalf("maintenance fence=%+v err=%v", fence, err)
	}
	if err := st.RegisterRAGCacheObject(ctx, shared); !errors.Is(err, ErrRAGDocumentMaintenanceActive) {
		t.Fatalf("cache refresh crossed maintenance fence: %v", err)
	}

	candidates, err := st.PruneRAGCacheCatalogAndListCleanupCandidates(
		ctx, *fence, time.Hour, 1, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]RAGCacheObjectCleanupCandidate, len(candidates))
	for _, candidate := range candidates {
		got[candidate.CacheKey] = candidate
	}
	for _, key := range []string{strings.Repeat("b", 64), strings.Repeat("c", 64)} {
		if _, ok := got[key]; !ok {
			t.Fatalf("expected expired/over-quota candidate %s, got %+v", key[:4], candidates)
		}
	}
	for _, key := range []string{shared.CacheKey, strings.Repeat("d", 64), strings.Repeat("e", 64)} {
		if _, ok := got[key]; ok {
			t.Fatalf("active/shared/recent generation became candidate %s", key[:4])
		}
	}
	var prunedSharedOldRef int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_cache_object_fingerprints
		WHERE doc_id=? AND fingerprint=?`, doc.ID, oldParse).Scan(&prunedSharedOldRef); err != nil {
		t.Fatal(err)
	}
	if prunedSharedOldRef != 0 {
		t.Fatalf("expired shared fingerprint refs=%d, want 0", prunedSharedOldRef)
	}

	expiredCandidate := got[strings.Repeat("b", 64)]
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_cache_objects
		SET generation=generation+1 WHERE doc_id=? AND cache_kind=? AND cache_key=?`,
		doc.ID, expiredCandidate.CacheKind, expiredCandidate.CacheKey); err != nil {
		t.Fatal(err)
	}
	if deleted, err := st.DeleteRAGCacheObjectWithMaintenance(ctx, *fence, expiredCandidate); err != nil || deleted {
		t.Fatalf("stale generation delete=%v err=%v", deleted, err)
	}
	candidates, err = st.PruneRAGCacheCatalogAndListCleanupCandidates(ctx, *fence, time.Hour, 1, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range candidates {
		if candidate.CacheKey == expiredCandidate.CacheKey {
			expiredCandidate = candidate
		}
	}
	if deleted, err := st.DeleteRAGCacheObjectWithMaintenance(ctx, *fence, expiredCandidate); err != nil || !deleted {
		t.Fatalf("current generation delete=%v err=%v", deleted, err)
	}

	var sharedGeneration int64
	if err := st.DB().QueryRowContext(ctx, `SELECT generation FROM rag_cache_objects
		WHERE doc_id=? AND cache_kind=? AND cache_key=?`, doc.ID, shared.CacheKind, shared.CacheKey).
		Scan(&sharedGeneration); err != nil {
		t.Fatal(err)
	}
	sharedCandidate := RAGCacheObjectCleanupCandidate{
		DocID: doc.ID, CacheKind: shared.CacheKind, CacheKey: shared.CacheKey,
		ObjectKey: shared.ObjectKey, Generation: sharedGeneration,
	}
	if deleted, err := st.DeleteRAGCacheObjectWithMaintenance(ctx, *fence, sharedCandidate); err != nil || deleted {
		t.Fatalf("active shared cache delete=%v err=%v", deleted, err)
	}
	if released, err := st.ReleaseRAGDocumentMaintenance(ctx, *fence); err != nil || !released {
		t.Fatalf("release maintenance=%v err=%v", released, err)
	}

	if err := tombstoneAndDeleteRAGDocumentForTest(ctx, st, doc.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"rag_cache_object_fingerprints", "rag_cache_objects"} {
		var count int
		if err := st.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE doc_id=?", doc.ID).
			Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s rows=%d err=%v", table, count, err)
		}
	}
}

func TestRAGCacheCatalogTTLStartsAtSuccessfulHitOrPut(t *testing.T) {
	ctx := context.Background()
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_cache_touch", 3)
	claim := activateRAGLifecycleVersion(t, st, "cache-touch-index", 0)
	record := testRAGCacheRecord(doc, RAGCacheKindPage, strings.Repeat("f", 64),
		RAGCacheFingerprintParse, strings.Repeat("9", 64))
	if err := st.RegisterRAGCacheObject(ctx, record); err != nil {
		t.Fatal(err)
	}
	old := time.Now().UTC().Add(-48 * time.Hour)
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_cache_objects SET updated_at=? WHERE doc_id=?`, old, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_cache_object_fingerprints SET updated_at=? WHERE doc_id=?`, old, doc.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterRAGCacheObject(ctx, record); err != nil {
		t.Fatal(err)
	}
	var updatedAt time.Time
	if err := st.DB().QueryRowContext(ctx, `SELECT updated_at FROM rag_cache_objects
		WHERE doc_id=? AND cache_kind=? AND cache_key=?`, doc.ID, record.CacheKind, record.CacheKey).
		Scan(&updatedAt); err != nil || !updatedAt.After(old) {
		t.Fatalf("cache hit/put timestamp=%v err=%v", updatedAt, err)
	}
	_ = claim
}

func TestRAGCacheFingerprintQuotaIsGlobalAndPrunesSharedCatalogRefs(t *testing.T) {
	ctx := context.Background()
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_cache_global_quota", 3)
	activateRAGLifecycleVersion(t, st, "cache-global-quota-index", 0)
	parseFingerprint := strings.Repeat("5", 64)
	indexFingerprint := strings.Repeat("6", 64)
	parseRecord := testRAGCacheRecord(doc, RAGCacheKindPage, strings.Repeat("7", 64),
		RAGCacheFingerprintParse, parseFingerprint)
	indexRecord := testRAGCacheRecord(doc, RAGCacheKindEnrich, strings.Repeat("8", 64),
		RAGCacheFingerprintIndex, indexFingerprint)
	for _, record := range []RAGCacheObjectRecord{parseRecord, indexRecord} {
		if err := st.RegisterRAGCacheObject(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	dbNow, err := st.ragDBNow(ctx, st.DB())
	if err != nil {
		t.Fatal(err)
	}
	for fingerprint, updatedAt := range map[string]time.Time{
		parseFingerprint: dbNow.Add(-time.Minute),
		indexFingerprint: dbNow.Add(-2 * time.Minute),
	} {
		if _, err := st.DB().ExecContext(ctx, `UPDATE rag_cache_object_fingerprints
			SET updated_at=? WHERE doc_id=? AND fingerprint=?`, updatedAt, doc.ID,
			fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	fence, err := st.ClaimRAGDocumentMaintenance(ctx, doc.ID, "cache-global-prune", time.Minute)
	if err != nil || fence == nil {
		t.Fatalf("maintenance=%+v err=%v", fence, err)
	}
	candidates, err := st.PruneRAGCacheCatalogAndListCleanupCandidates(
		ctx, *fence, time.Hour, 1, 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].CacheKey != indexRecord.CacheKey {
		t.Fatalf("global quota candidates=%+v", candidates)
	}
	var parseRefs, indexRefs int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_cache_object_fingerprints
		WHERE doc_id=? AND fingerprint=?`, doc.ID, parseFingerprint).Scan(&parseRefs); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_cache_object_fingerprints
		WHERE doc_id=? AND fingerprint=?`, doc.ID, indexFingerprint).Scan(&indexRefs); err != nil {
		t.Fatal(err)
	}
	if parseRefs != 1 || indexRefs != 0 {
		t.Fatalf("global fingerprint refs parse=%d index=%d", parseRefs, indexRefs)
	}
	if released, err := st.ReleaseRAGDocumentMaintenance(ctx, *fence); err != nil || !released {
		t.Fatalf("release=%v err=%v", released, err)
	}
}

func TestRAGCacheCatalogDocumentListingUsesKeysetCursor(t *testing.T) {
	ctx := context.Background()
	st := openRAGTaskClaimStore(t)
	docA, _ := seedRAGTaskDocument(t, st, "doc_cache_cursor_a", 3)
	docB, _ := seedRAGTaskDocument(t, st, "doc_cache_cursor_b", 3)
	for i, doc := range []*RAGDocumentRecord{docA, docB} {
		record := testRAGCacheRecord(doc, RAGCacheKindPage,
			strings.Repeat(string(rune('a'+i)), 64), RAGCacheFingerprintParse,
			strings.Repeat(string(rune('1'+i)), 64))
		if err := st.RegisterRAGCacheObject(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.ListRAGCacheCatalogDocuments(ctx, "", 1)
	if err != nil || len(first) != 1 || first[0] != docA.ID {
		t.Fatalf("first page=%v err=%v", first, err)
	}
	second, err := st.ListRAGCacheCatalogDocuments(ctx, first[0], 1)
	if err != nil || len(second) != 1 || second[0] != docB.ID {
		t.Fatalf("second page=%v err=%v", second, err)
	}
	end, err := st.ListRAGCacheCatalogDocuments(ctx, second[0], 1)
	if err != nil || len(end) != 0 {
		t.Fatalf("end page=%v err=%v", end, err)
	}
}
