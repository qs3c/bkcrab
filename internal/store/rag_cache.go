package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	RAGCacheKindPage   = "page"
	RAGCacheKindImage  = "image"
	RAGCacheKindEnrich = "enrich"

	RAGCacheFingerprintParse = "parse"
	RAGCacheFingerprintIndex = "index"
)

// RAGCacheObjectRecord is one deterministic object-store cache entry and one
// immutable version-fingerprint association. Multiple associations may point
// at the same object; this is how a content-addressed page/image result can be
// shared safely by active and staging parse generations.
type RAGCacheObjectRecord struct {
	DocID           string
	CacheKind       string
	CacheKey        string
	ObjectKey       string
	FingerprintKind string
	Fingerprint     string
	Generation      int64
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// RAGCacheObjectCleanupCandidate snapshots the object generation used by the
// lifecycle worker's external-delete then catalog-CAS sequence.
type RAGCacheObjectCleanupCandidate struct {
	DocID      string
	CacheKind  string
	CacheKey   string
	ObjectKey  string
	Generation int64
	UpdatedAt  time.Time
}

// RAGCacheCatalog is the narrow dependency injected into page/image and text
// ObjectCache implementations. Register is used for both successful puts and
// validated hits, so updated_at is a durable last-use clock.
type RAGCacheCatalog interface {
	RegisterRAGCacheObject(context.Context, RAGCacheObjectRecord) error
}

type ragCacheObjectIdentity struct {
	kind string
	key  string
}

type ragCacheFingerprintIdentity struct {
	kind        string
	fingerprint string
}

type ragCacheFingerprintRef struct {
	object      ragCacheObjectIdentity
	fingerprint ragCacheFingerprintIdentity
	updatedAt   time.Time
}

func ragCacheFingerprintKind(cacheKind string) (string, bool) {
	switch cacheKind {
	case RAGCacheKindPage, RAGCacheKindImage:
		return RAGCacheFingerprintParse, true
	case RAGCacheKindEnrich:
		return RAGCacheFingerprintIndex, true
	default:
		return "", false
	}
}

func ragCacheDirectory(cacheKind string) (string, bool) {
	switch cacheKind {
	case RAGCacheKindPage:
		return "pages", true
	case RAGCacheKindImage:
		return "images", true
	case RAGCacheKindEnrich:
		return "enrich", true
	default:
		return "", false
	}
}

func validateRAGCacheObjectRecord(record RAGCacheObjectRecord) error {
	record.DocID = strings.TrimSpace(record.DocID)
	record.CacheKind = strings.TrimSpace(record.CacheKind)
	record.CacheKey = strings.TrimSpace(record.CacheKey)
	record.ObjectKey = strings.TrimSpace(record.ObjectKey)
	record.FingerprintKind = strings.TrimSpace(record.FingerprintKind)
	record.Fingerprint = strings.TrimSpace(record.Fingerprint)
	wantFingerprintKind, ok := ragCacheFingerprintKind(record.CacheKind)
	if record.DocID == "" || !ok || record.FingerprintKind != wantFingerprintKind ||
		!ragCanonicalSHA256(record.CacheKey) || !ragCanonicalSHA256(record.Fingerprint) ||
		record.ObjectKey == "" || record.ObjectKey != strings.ReplaceAll(record.ObjectKey, "\\", "/") {
		return ErrRAGDocumentVersionMismatch
	}
	return nil
}

func expectedRAGCacheObjectKey(route ragOwnershipRoute, docID, cacheKind, cacheKey string) (string, error) {
	directory, ok := ragCacheDirectory(cacheKind)
	if !ok {
		return "", ErrRAGDocumentVersionMismatch
	}
	return fmt.Sprintf("rag/%s/%s/%s/cache/%s/%s.json",
		route.UserID, route.KBID, docID, directory, cacheKey), nil
}

// RegisterRAGCacheObject creates or refreshes one object and its immutable
// parse/index generation association. The document hierarchy lock and active
// maintenance check serialize cache IO with destructive lifecycle work across
// service instances.
func (d *DBStore) RegisterRAGCacheObject(ctx context.Context, record RAGCacheObjectRecord) error {
	record.DocID = strings.TrimSpace(record.DocID)
	record.CacheKind = strings.TrimSpace(record.CacheKind)
	record.CacheKey = strings.TrimSpace(record.CacheKey)
	record.ObjectKey = strings.TrimSpace(record.ObjectKey)
	record.FingerprintKind = strings.TrimSpace(record.FingerprintKind)
	record.Fingerprint = strings.TrimSpace(record.Fingerprint)
	if err := validateRAGCacheObjectRecord(record); err != nil {
		return err
	}
	route, err := d.ragOwnershipRoute(ctx, record.DocID)
	if errors.Is(err, ErrNotFound) {
		return ErrRAGLifecycleInactive
	}
	if err != nil {
		return err
	}
	expectedObjectKey, err := expectedRAGCacheObjectKey(
		route, record.DocID, record.CacheKind, record.CacheKey,
	)
	if err != nil || record.ObjectKey != expectedObjectKey {
		return ErrRAGDocumentVersionMismatch
	}

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, active, err := d.lockRAGDocumentHierarchyTx(ctx, tx, record.DocID, route); err != nil {
		return err
	} else if !active {
		return ErrRAGLifecycleInactive
	}
	if err := d.rejectActiveRAGDocumentMaintenanceInTx(ctx, tx, record.DocID); err != nil {
		return err
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return err
	}

	var objectKey string
	var generation int64
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT object_key,generation
		FROM rag_cache_objects WHERE doc_id=%s AND cache_kind=%s AND cache_key=%s%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ragLockSuffix()), record.DocID,
		record.CacheKind, record.CacheKey).Scan(&objectKey, &generation)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		generation = 1
		if _, err = tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_cache_objects
			(doc_id,cache_kind,cache_key,object_key,generation,created_at,updated_at)
			VALUES (%s,%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4),
			d.ph(5), d.ph(6), d.ph(7)), record.DocID, record.CacheKind,
			record.CacheKey, record.ObjectKey, generation, now, now); err != nil {
			return err
		}
	case err != nil:
		return err
	case objectKey != record.ObjectKey:
		return ErrRAGDocumentVersionMismatch
	default:
		generation++
		result, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_cache_objects SET
			generation=%s,updated_at=%s WHERE doc_id=%s AND cache_kind=%s AND cache_key=%s
			AND generation=%s AND object_key=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4),
			d.ph(5), d.ph(6), d.ph(7)), generation, now, record.DocID,
			record.CacheKind, record.CacheKey, generation-1, record.ObjectKey)
		if updateErr != nil {
			return updateErr
		}
		if updated, updateErr := ragRowsAffected(result); updateErr != nil || !updated {
			return updateErr
		}
	}

	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_cache_object_fingerprints
		SET updated_at=%s WHERE doc_id=%s AND cache_kind=%s AND cache_key=%s
		AND fingerprint_kind=%s AND fingerprint=%s`, d.ph(1), d.ph(2), d.ph(3),
		d.ph(4), d.ph(5), d.ph(6)), now, record.DocID, record.CacheKind,
		record.CacheKey, record.FingerprintKind, record.Fingerprint)
	if err != nil {
		return err
	}
	updated, err := ragRowsAffected(result)
	if err != nil {
		return err
	}
	if !updated {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_cache_object_fingerprints
			(doc_id,cache_kind,cache_key,fingerprint_kind,fingerprint,updated_at)
			VALUES (%s,%s,%s,%s,%s,%s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4),
			d.ph(5), d.ph(6)), record.DocID, record.CacheKind, record.CacheKey,
			record.FingerprintKind, record.Fingerprint, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DBStore) ListRAGCacheCatalogDocuments(
	ctx context.Context,
	afterDocID string,
	limit int,
) ([]string, error) {
	afterDocID = strings.TrimSpace(afterDocID)
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(`SELECT DISTINCT c.doc_id
		FROM rag_cache_objects c JOIN rag_documents d ON d.id=c.doc_id
		JOIN rag_kbs kb ON kb.id=d.kb_id JOIN users u ON u.id=kb.user_id
		WHERE c.doc_id>%s AND UPPER(d.status)<>'DELETING' AND LOWER(kb.status)='active'
		AND LOWER(u.status)='active' ORDER BY c.doc_id LIMIT %s`, d.ph(1), d.ph(2)),
		afterDocID, ragCleanupListLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	docIDs := make([]string, 0)
	for rows.Next() {
		var docID string
		if err := rows.Scan(&docID); err != nil {
			return nil, err
		}
		docIDs = append(docIDs, docID)
	}
	return docIDs, rows.Err()
}

// PruneRAGCacheCatalogAndListCleanupCandidates enforces one global
// per-document parse/index fingerprint budget using database time, removes
// expired fingerprint associations, and returns objects that no longer have
// any association. The caller holds a maintenance lease, so cache hit/put
// registration cannot race this catalog pruning or the subsequent external
// object deletion.
func (d *DBStore) PruneRAGCacheCatalogAndListCleanupCandidates(
	ctx context.Context,
	fence RAGDocumentMaintenanceFence,
	staleFor time.Duration,
	maxUnreferencedFingerprints int,
	limit int,
) ([]RAGCacheObjectCleanupCandidate, error) {
	if staleFor < 0 {
		return nil, errors.New("store: RAG cache stale duration must not be negative")
	}
	route, err := d.ragOwnershipRoute(ctx, fence.DocID)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	doc, active, err := d.lockRAGDocumentHierarchyTx(ctx, tx, fence.DocID, route)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, nil
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	olderThan := now.Add(-staleFor)
	valid, err := d.ragDocumentMaintenanceFenceValidInTx(ctx, tx, fence, now)
	if err != nil || !valid {
		return nil, err
	}

	activeFingerprints := map[string]string{}
	if doc.ActiveVersion > 0 {
		var parseFingerprint, indexFingerprint string
		err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT parse_fingerprint,index_fingerprint
			FROM rag_document_versions WHERE doc_id=%s AND doc_version=%s`,
			d.ph(1), d.ph(2)), doc.ID, doc.ActiveVersion).
			Scan(&parseFingerprint, &indexFingerprint)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		if err == nil {
			activeFingerprints[RAGCacheFingerprintParse] = parseFingerprint
			activeFingerprints[RAGCacheFingerprintIndex] = indexFingerprint
		}
	}

	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT cache_kind,cache_key,object_key,
		generation,created_at,updated_at FROM rag_cache_objects WHERE doc_id=%s
		ORDER BY updated_at,cache_kind,cache_key`, d.ph(1)), doc.ID)
	if err != nil {
		return nil, err
	}
	objects := make(map[ragCacheObjectIdentity]RAGCacheObjectCleanupCandidate)
	for rows.Next() {
		var candidate RAGCacheObjectCleanupCandidate
		candidate.DocID = doc.ID
		var createdAt time.Time
		if err := rows.Scan(&candidate.CacheKind, &candidate.CacheKey, &candidate.ObjectKey,
			&candidate.Generation, &createdAt, &candidate.UpdatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		objects[ragCacheObjectIdentity{kind: candidate.CacheKind, key: candidate.CacheKey}] = candidate
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = tx.QueryContext(ctx, fmt.Sprintf(`SELECT cache_kind,cache_key,
		fingerprint_kind,fingerprint,updated_at FROM rag_cache_object_fingerprints
		WHERE doc_id=%s`, d.ph(1)), doc.ID)
	if err != nil {
		return nil, err
	}
	refs := make([]ragCacheFingerprintRef, 0)
	latest := make(map[ragCacheFingerprintIdentity]time.Time)
	for rows.Next() {
		var ref ragCacheFingerprintRef
		if err := rows.Scan(&ref.object.kind, &ref.object.key, &ref.fingerprint.kind,
			&ref.fingerprint.fingerprint, &ref.updatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
		if previous, ok := latest[ref.fingerprint]; !ok || ref.updatedAt.After(previous) {
			latest[ref.fingerprint] = ref.updatedAt
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	unreferenced := make([]ragCacheFingerprintIdentity, 0, len(latest))
	for fingerprint := range latest {
		if fingerprint.fingerprint == activeFingerprints[fingerprint.kind] {
			continue
		}
		unreferenced = append(unreferenced, fingerprint)
	}
	sort.Slice(unreferenced, func(i, j int) bool {
		left, right := latest[unreferenced[i]], latest[unreferenced[j]]
		if left.Equal(right) {
			if unreferenced[i].kind == unreferenced[j].kind {
				return unreferenced[i].fingerprint < unreferenced[j].fingerprint
			}
			return unreferenced[i].kind < unreferenced[j].kind
		}
		return left.After(right)
	})
	retained := make(map[ragCacheFingerprintIdentity]bool, len(latest))
	for kind, fingerprint := range activeFingerprints {
		if fingerprint != "" {
			retained[ragCacheFingerprintIdentity{kind: kind, fingerprint: fingerprint}] = true
		}
	}
	for i, fingerprint := range unreferenced {
		withinQuota := maxUnreferencedFingerprints < 0 || i < maxUnreferencedFingerprints
		if withinQuota && latest[fingerprint].After(olderThan) {
			retained[fingerprint] = true
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_cache_object_fingerprints
			WHERE doc_id=%s AND fingerprint_kind=%s AND fingerprint=%s`,
			d.ph(1), d.ph(2), d.ph(3)), doc.ID, fingerprint.kind,
			fingerprint.fingerprint); err != nil {
			return nil, err
		}
	}

	hasRetainedRef := make(map[ragCacheObjectIdentity]bool, len(objects))
	for _, ref := range refs {
		if _, exists := objects[ref.object]; !exists {
			continue
		}
		if retained[ref.fingerprint] {
			hasRetainedRef[ref.object] = true
		}
	}

	candidates := make([]RAGCacheObjectCleanupCandidate, 0)
	for object, candidate := range objects {
		if !hasRetainedRef[object] {
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			if candidates[i].CacheKind == candidates[j].CacheKind {
				return candidates[i].CacheKey < candidates[j].CacheKey
			}
			return candidates[i].CacheKind < candidates[j].CacheKind
		}
		return candidates[i].UpdatedAt.Before(candidates[j].UpdatedAt)
	})
	limit = ragCleanupListLimit(limit)
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return candidates, nil
}

// DeleteRAGCacheObjectWithMaintenance is the catalog half of exact object
// cleanup. The lifecycle worker checks the same maintenance fence immediately
// before and after external deletion; this transaction then revalidates active
// fingerprint sharing and CAS-deletes only the observed object generation.
func (d *DBStore) DeleteRAGCacheObjectWithMaintenance(
	ctx context.Context,
	fence RAGDocumentMaintenanceFence,
	candidate RAGCacheObjectCleanupCandidate,
) (bool, error) {
	if candidate.DocID != fence.DocID || !ragCanonicalSHA256(candidate.CacheKey) ||
		candidate.Generation <= 0 || strings.TrimSpace(candidate.ObjectKey) == "" {
		return false, ErrRAGDocumentVersionMismatch
	}
	route, err := d.ragOwnershipRoute(ctx, fence.DocID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	_, active, err := d.lockRAGDocumentHierarchyTx(ctx, tx, fence.DocID, route)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !active {
		return false, nil
	}
	now, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return false, err
	}
	valid, err := d.ragDocumentMaintenanceFenceValidInTx(ctx, tx, fence, now)
	if err != nil || !valid {
		return false, err
	}

	var objectKey string
	var generation int64
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT object_key,generation FROM rag_cache_objects
		WHERE doc_id=%s AND cache_kind=%s AND cache_key=%s%s`, d.ph(1), d.ph(2),
		d.ph(3), d.ragLockSuffix()), candidate.DocID, candidate.CacheKind,
		candidate.CacheKey).Scan(&objectKey, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if objectKey != candidate.ObjectKey || generation != candidate.Generation {
		return false, nil
	}
	var references int
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*)
		FROM rag_cache_object_fingerprints WHERE doc_id=%s AND cache_kind=%s
		AND cache_key=%s`, d.ph(1), d.ph(2), d.ph(3)), candidate.DocID,
		candidate.CacheKind, candidate.CacheKey).Scan(&references); err != nil {
		return false, err
	}
	if references != 0 {
		return false, nil
	}
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_cache_objects
		WHERE doc_id=%s AND cache_kind=%s AND cache_key=%s AND generation=%s
		AND object_key=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5)),
		candidate.DocID, candidate.CacheKind, candidate.CacheKey,
		candidate.Generation, candidate.ObjectKey)
	if err != nil {
		return false, err
	}
	deleted, err := ragRowsAffected(result)
	if err != nil || !deleted {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_cache_object_fingerprints
		WHERE doc_id=%s AND cache_kind=%s AND cache_key=%s`, d.ph(1), d.ph(2), d.ph(3)),
		candidate.DocID, candidate.CacheKind, candidate.CacheKey); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
