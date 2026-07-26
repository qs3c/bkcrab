package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openUnmigratedRAGSQLite(t *testing.T) *DBStore {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "legacy-rag.db") +
		"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	st, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open legacy sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func installLegacyRAGSchema(t *testing.T, st *DBStore) {
	t.Helper()
	ctx := context.Background()
	statements := []string{
		`CREATE TABLE rag_kbs (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			embed_provider TEXT NOT NULL DEFAULT 'system',
			embed_model TEXT NOT NULL,
			embed_dims INTEGER NOT NULL,
			chunk_size INTEGER NOT NULL DEFAULT 512,
			chunk_overlap INTEGER NOT NULL DEFAULT 64,
			status TEXT NOT NULL DEFAULT 'active',
			created_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE rag_chat_turns (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			kb_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			title TEXT NOT NULL DEFAULT '',
			question TEXT NOT NULL,
			answer TEXT NOT NULL,
			sources TEXT NOT NULL DEFAULT '[]',
			created_at TIMESTAMP NOT NULL
		)`,
		`CREATE TABLE rag_documents (
			id TEXT PRIMARY KEY,
			kb_id TEXT NOT NULL,
			file_name TEXT NOT NULL,
			file_type TEXT NOT NULL,
			file_size BIGINT NOT NULL DEFAULT 0,
			object_key TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'PENDING',
			error_msg TEXT NOT NULL DEFAULT '',
			chunk_count INTEGER NOT NULL DEFAULT 0,
			token_count INTEGER NOT NULL DEFAULT 0,
			version INTEGER NOT NULL DEFAULT 1,
			uploaded_at TIMESTAMP NOT NULL,
			indexed_at TIMESTAMP
		)`,
		`CREATE TABLE rag_index_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			doc_id TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'PENDING',
			retry_count INTEGER NOT NULL DEFAULT 0,
			max_retry INTEGER NOT NULL DEFAULT 3,
			error_msg TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMP NOT NULL,
			started_at TIMESTAMP,
			finished_at TIMESTAMP
		)`,
	}
	for _, statement := range statements {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("install legacy RAG schema: %v\nSQL: %s", err, statement)
		}
	}
}

func TestRAGMigrationFromLegacySchemaIsIdempotent(t *testing.T) {
	st := openUnmigratedRAGSQLite(t)
	installLegacyRAGSchema(t, st)
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 1, 2, 3, 0, time.UTC)

	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_kbs
		(id,user_id,name,description,embed_provider,embed_model,embed_dims,chunk_size,chunk_overlap,status,created_at,updated_at)
		VALUES ('kb_legacy','u_legacy','legacy','','system','embed-v1',768,512,64,'active',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_documents
		(id,kb_id,file_name,file_type,file_size,object_key,status,error_msg,chunk_count,token_count,version,uploaded_at,indexed_at)
		VALUES ('doc_done','kb_legacy','done.md','md',10,'rag/u/kb/doc/done.md','DONE','',3,99,7,?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_documents
		(id,kb_id,file_name,file_type,file_size,object_key,status,error_msg,chunk_count,token_count,version,uploaded_at,indexed_at)
		VALUES ('doc_pending','kb_legacy','pending.md','md',11,'rag/u/kb/doc/pending.md','PENDING','',0,0,8,?,NULL)`, now); err != nil {
		t.Fatal(err)
	}
	legacySources := json.RawMessage(`[{"docId":"doc_done","content":"legacy source"}]`)
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_chat_turns
		(id,user_id,kb_id,session_id,title,question,answer,sources,created_at)
		VALUES ('turn_legacy','u_legacy','kb_legacy','sess','legacy','q','a',?,?)`, string(legacySources), now); err != nil {
		t.Fatal(err)
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate pass 1: %v", err)
	}
	ensureRAGLifecycleUser(t, st, "u_legacy", "active")
	// A current-format DONE row whose snapshot is missing is corrupt, not
	// legacy. A later migration must not bless it as a visible legacy-v0 index.
	currentWithoutSnapshot := &RAGDocumentRecord{
		ID: "doc_current_without_snapshot", KBID: "kb_legacy", FileName: "current.md",
		FileType: "md", ObjectKey: "rag/u/kb/doc/current.md", Status: "DONE",
		Version: 12, SourceSHA256: strings.Repeat("a", 64), UploadedAt: now, IndexedAt: &now,
	}
	if err := st.CreateRAGDocument(ctx, currentWithoutSnapshot); err != nil {
		t.Fatalf("insert current-format corrupt fixture: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate pass 2: %v", err)
	}

	for table, columns := range map[string][]string{
		"rag_kbs": {
			"parse_mode", "enrichment_enabled", "provisioning_generation",
			"provisioning_lease_owner", "provisioning_lease_until",
		},
		"rag_assets": {"thumbnail_sha256"},
		"rag_documents": {
			"source_sha256", "active_version", "index_format_version", "processing_stage",
			"progress_current", "progress_total", "progress_unit", "degraded", "warning_count",
		},
	} {
		for _, column := range columns {
			has, err := st.tableHasColumn(ctx, table, column)
			if err != nil {
				t.Fatalf("inspect %s.%s: %v", table, column, err)
			}
			if !has {
				t.Errorf("migration did not add %s.%s", table, column)
			}
		}
	}
	for _, table := range []string{
		"rag_document_versions", "rag_assets", "rag_version_assets", "rag_document_maintenance_leases",
		"rag_cache_objects", "rag_cache_object_fingerprints",
		"rag_chunks", "rag_chunk_assets",
		"rag_index_gc_tasks", "rag_document_ai_task_budgets",
		"rag_document_ai_user_budgets", "rag_document_ai_usage",
	} {
		exists, err := st.tableExists(ctx, table)
		if err != nil {
			t.Fatalf("inspect table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("migration did not create %s", table)
		}
	}
	if found, unique, columns := ragTaskMigrationIndex(t, st, "rag_version_assets", "idx_rag_version_assets_asset"); !found || unique || strings.Join(columns, ",") != "asset_id,doc_id,doc_version" {
		t.Fatalf("version asset lookup index found=%v unique=%v columns=%v", found, unique, columns)
	}
	if found, unique, columns := ragTaskMigrationIndex(t, st, "rag_document_maintenance_leases", "idx_rag_document_maintenance_lease_until"); !found || unique || strings.Join(columns, ",") != "lease_until" {
		t.Fatalf("document maintenance lease index found=%v unique=%v columns=%v", found, unique, columns)
	}
	if found, unique, columns := ragTaskMigrationIndex(t, st, "rag_cache_objects", "idx_rag_cache_objects_doc_updated"); !found || unique || strings.Join(columns, ",") != "doc_id,updated_at" {
		t.Fatalf("cache object cleanup index found=%v unique=%v columns=%v", found, unique, columns)
	}
	if found, unique, columns := ragTaskMigrationIndex(t, st, "rag_cache_object_fingerprints", "idx_rag_cache_fingerprints_generation"); !found || unique || strings.Join(columns, ",") != "doc_id,fingerprint_kind,fingerprint,updated_at" {
		t.Fatalf("cache fingerprint generation index found=%v unique=%v columns=%v", found, unique, columns)
	}

	kb, err := st.GetRAGKB(ctx, "kb_legacy")
	if err != nil {
		t.Fatal(err)
	}
	if kb.ParseMode != RAGParseModeStandard || kb.EnrichmentEnabled {
		t.Fatalf("legacy KB config = parseMode %q enrichment %v", kb.ParseMode, kb.EnrichmentEnabled)
	}
	done, err := st.GetRAGDocument(ctx, "doc_done")
	if err != nil {
		t.Fatal(err)
	}
	if done.Version != 7 || done.ActiveVersion != 7 || done.IndexFormatVersion != 0 {
		t.Fatalf("legacy DONE pin = version %d active %d format %d", done.Version, done.ActiveVersion, done.IndexFormatVersion)
	}
	pending, err := st.GetRAGDocument(ctx, "doc_pending")
	if err != nil {
		t.Fatal(err)
	}
	if pending.ActiveVersion != 0 || pending.IndexFormatVersion != 1 {
		t.Fatalf("legacy pending pin = active %d format %d", pending.ActiveVersion, pending.IndexFormatVersion)
	}
	current, err := st.GetRAGDocument(ctx, currentWithoutSnapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.ActiveVersion != 0 || current.IndexFormatVersion != 1 {
		t.Fatalf("current corrupt row was incorrectly legacy-pinned: %+v", current)
	}
	if _, err := st.GetRAGDocumentVersion(ctx, current.ID, current.Version); !errors.Is(err, ErrNotFound) {
		t.Fatalf("current corrupt row gained a synthetic version: %v", err)
	}

	version, err := st.GetRAGDocumentVersion(ctx, "doc_done", 7)
	if err != nil {
		t.Fatalf("synthetic legacy version: %v", err)
	}
	if version.Status != RAGDocumentVersionDone || version.ParserVersion != ragLegacyVersionSentinel ||
		version.ParseMode != RAGParseModeStandard || version.EnrichmentEnabled ||
		version.EmbeddingProvider != "system" || version.EmbeddingModel != "embed-v1" ||
		version.EmbeddingDimensions != 768 || version.ChunkSize != 512 || version.ChunkOverlap != 64 ||
		version.ParseArtifactKey != "" || version.MaxDocumentAIRequests != 300 ||
		version.MaxDocumentAITokens != 200000 || version.MaxDocumentAICostMicroUSD != 1_000_000 {
		t.Fatalf("synthetic legacy version = %+v", version)
	}
	versions, err := st.ListRAGDocumentVersions(ctx, "doc_done")
	if err != nil || len(versions) != 1 {
		t.Fatalf("legacy versions = %+v err=%v", versions, err)
	}

	turns, err := st.ListRAGChatTurns(ctx, "u_legacy", "kb_legacy", "sess")
	if err != nil || len(turns) != 1 || string(turns[0].Sources) != string(legacySources) {
		t.Fatalf("legacy chat sources changed: %+v err=%v", turns, err)
	}

	var declaredVersionType string
	rows, err := st.db.QueryContext(ctx, `PRAGMA table_info(rag_documents)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid, notNull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &pk); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if name == "version" {
			declaredVersionType = strings.ToUpper(columnType)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if declaredVersionType != "BIGINT" {
		t.Fatalf("rag_documents.version declared type = %q, want BIGINT", declaredVersionType)
	}
}

func TestRAGVersionAssetMigrationBackfillsActiveLegacyAssetOnce(t *testing.T) {
	st := openUnmigratedRAGSQLite(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ensureRAGLifecycleUser(t, st, "u_version_asset_migration", "active")
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.CreateRAGKB(ctx, &RAGKBRecord{
		ID: "kb_version_asset_migration", UserID: "u_version_asset_migration", Name: "migration",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 8,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard, Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	doc := &RAGDocumentRecord{
		ID: "doc_version_asset_migration", KBID: "kb_version_asset_migration",
		FileName: "migration.pdf", FileType: "pdf", ObjectKey: "source/migration.pdf",
		Status: "PENDING", Version: 1, UploadedAt: now,
	}
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, testRAGDocumentVersion(doc.ID, 1), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_document_versions SET status='DONE'
		WHERE doc_id=? AND doc_version=1`, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_documents SET status='DONE',active_version=1 WHERE id=?`, doc.ID); err != nil {
		t.Fatal(err)
	}
	asset := &RAGAssetRecord{
		ID: "ast_legacy_version_asset", DocID: doc.ID,
		ContentSHA256: strings.Repeat("a", 64), SourceKind: "embedded_original", SourceMIME: "image/png",
		SourceObjectKey: "source/asset.png", DisplayStatus: "unavailable", ByteSize: 1, Width: 1, Height: 1,
		FirstSeenVersion: 1, LastSeenVersion: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}
	var markerCount int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migration_markers
		WHERE name=?`, ragVersionAssetsBackfillMigration).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 1 {
		t.Fatalf("initial RAG version asset backfill markers = %d, want 1", markerCount)
	}
	if _, err := st.DB().ExecContext(ctx, `DROP TABLE rag_version_assets`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate exact asset mapping: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("idempotent migration: %v", err)
	}
	var count int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_version_assets
		WHERE doc_id=? AND doc_version=1 AND asset_id=?`, doc.ID, asset.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("active legacy version asset mappings = %d, want 1", count)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migration_markers
		WHERE name=?`, ragVersionAssetsBackfillMigration).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 1 {
		t.Fatalf("recreated-table backfill markers = %d, want 1", markerCount)
	}
	if found, unique, columns := ragTaskMigrationIndex(t, st, "rag_version_assets", "idx_rag_version_assets_asset"); !found || unique || strings.Join(columns, ",") != "asset_id,doc_id,doc_version" {
		t.Fatalf("version asset lookup index found=%v unique=%v columns=%v", found, unique, columns)
	}
}

func TestRAGVersionAssetMigrationRetriesAfterBackfillFailure(t *testing.T) {
	st := openUnmigratedRAGSQLite(t)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	ensureRAGLifecycleUser(t, st, "u_version_asset_retry", "active")
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := st.CreateRAGKB(ctx, &RAGKBRecord{
		ID: "kb_version_asset_retry", UserID: "u_version_asset_retry", Name: "retry",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 8,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard, Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	doc := &RAGDocumentRecord{
		ID: "doc_version_asset_retry", KBID: "kb_version_asset_retry",
		FileName: "retry.pdf", FileType: "pdf", ObjectKey: "source/retry.pdf",
		Status: "PENDING", Version: 1, UploadedAt: now,
	}
	if _, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, testRAGDocumentVersion(doc.ID, 1), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_document_versions SET status='DONE'
		WHERE doc_id=? AND doc_version=1`, doc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().ExecContext(ctx, `UPDATE rag_documents SET status='DONE',active_version=1 WHERE id=?`, doc.ID); err != nil {
		t.Fatal(err)
	}
	asset := &RAGAssetRecord{
		ID: "ast_legacy_version_asset_retry", DocID: doc.ID,
		ContentSHA256: strings.Repeat("b", 64), SourceKind: "embedded_original", SourceMIME: "image/png",
		SourceObjectKey: "source/retry-asset.png", DisplayStatus: "unavailable", ByteSize: 1, Width: 1, Height: 1,
		FirstSeenVersion: 1, LastSeenVersion: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := st.UpsertRAGAsset(ctx, asset); err != nil {
		t.Fatal(err)
	}

	// Model the durable state left after canonical DDL succeeded but the
	// transactional backfill failed: the target table exists and the completion
	// marker does not.
	if _, err := st.DB().ExecContext(ctx, `DROP TABLE rag_version_assets`); err != nil {
		t.Fatalf("drop exact mapping table: %v", err)
	}
	if _, err := st.DB().ExecContext(ctx, `DELETE FROM schema_migration_markers WHERE name=?`,
		ragVersionAssetsBackfillMigration); err != nil {
		t.Fatalf("clear completion marker: %v", err)
	}
	for _, statement := range []string{
		st.ragVersionAssetsTableSQL(),
		`CREATE INDEX idx_rag_version_assets_asset ON rag_version_assets (asset_id, doc_id, doc_version)`,
		`CREATE TRIGGER reject_version_asset_backfill BEFORE INSERT ON rag_version_assets
			BEGIN SELECT RAISE(ABORT, 'forced version asset backfill failure'); END`,
	} {
		if _, err := st.DB().ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare failed backfill: %v\nSQL: %s", err, statement)
		}
	}

	if err := st.Migrate(ctx); err == nil || !strings.Contains(err.Error(), "forced version asset backfill failure") {
		t.Fatalf("failed backfill error = %v, want forced trigger error", err)
	}
	var markerCount int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migration_markers
		WHERE name=?`, ragVersionAssetsBackfillMigration).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 0 {
		t.Fatalf("completion markers after failed backfill = %d, want 0", markerCount)
	}
	var mappingCount int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_version_assets
		WHERE doc_id=? AND doc_version=1 AND asset_id=?`, doc.ID, asset.ID).Scan(&mappingCount); err != nil {
		t.Fatal(err)
	}
	if mappingCount != 0 {
		t.Fatalf("legacy version asset mappings after failed backfill = %d, want 0", mappingCount)
	}
	if _, err := st.DB().ExecContext(ctx, `DROP TRIGGER reject_version_asset_backfill`); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("retry migration: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("idempotent retry migration: %v", err)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_version_assets
		WHERE doc_id=? AND doc_version=1 AND asset_id=?`, doc.ID, asset.ID).Scan(&mappingCount); err != nil {
		t.Fatal(err)
	}
	if mappingCount != 1 {
		t.Fatalf("retried legacy version asset mappings = %d, want 1", mappingCount)
	}
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migration_markers
		WHERE name=?`, ragVersionAssetsBackfillMigration).Scan(&markerCount); err != nil {
		t.Fatal(err)
	}
	if markerCount != 1 {
		t.Fatalf("completion markers after successful retry = %d, want 1", markerCount)
	}
}

func TestRAGCanonicalDDLContainsAllPhaseATablesForBothFamilies(t *testing.T) {
	sqlitePostgres := strings.Join((&DBStore{dialect: "sqlite"}).migrationSQL(), "\n")
	mysql := strings.Join(mysqlMigrationSQL(), "\n")
	for _, ddl := range []struct {
		name string
		sql  string
	}{{"sqlite/postgres", sqlitePostgres}, {"mysql", mysql}} {
		for _, token := range []string{
			"schema_migration_markers", "parse_mode", "active_version", "rag_document_versions", "rag_assets",
			"thumbnail_sha256", "rag_version_assets", "idx_rag_version_assets_asset",
			"rag_document_maintenance_leases", "idx_rag_document_maintenance_lease_until",
			"rag_cache_objects", "idx_rag_cache_objects_doc_updated",
			"rag_cache_object_fingerprints", "idx_rag_cache_fingerprints_generation",
			"rag_chunks", "rag_chunk_assets", "rag_index_gc_tasks",
			"rag_document_ai_task_budgets", "rag_document_ai_user_budgets", "rag_document_ai_usage",
		} {
			if !strings.Contains(ddl.sql, token) {
				t.Errorf("%s canonical DDL missing %q", ddl.name, token)
			}
		}
	}
}
