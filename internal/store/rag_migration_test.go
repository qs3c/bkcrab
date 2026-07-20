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
		"rag_kbs":    {"parse_mode", "enrichment_enabled"},
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
		"rag_document_versions", "rag_assets", "rag_chunks", "rag_chunk_assets",
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

func TestRAGCanonicalDDLContainsAllPhaseATablesForBothFamilies(t *testing.T) {
	sqlitePostgres := strings.Join((&DBStore{dialect: "sqlite"}).migrationSQL(), "\n")
	mysql := strings.Join(mysqlMigrationSQL(), "\n")
	for _, ddl := range []struct {
		name string
		sql  string
	}{{"sqlite/postgres", sqlitePostgres}, {"mysql", mysql}} {
		for _, token := range []string{
			"parse_mode", "active_version", "rag_document_versions", "rag_assets",
			"thumbnail_sha256", "rag_chunks", "rag_chunk_assets", "rag_index_gc_tasks",
			"rag_document_ai_task_budgets", "rag_document_ai_user_budgets", "rag_document_ai_usage",
		} {
			if !strings.Contains(ddl.sql, token) {
				t.Errorf("%s canonical DDL missing %q", ddl.name, token)
			}
		}
	}
}
