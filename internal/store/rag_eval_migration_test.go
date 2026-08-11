package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

var ragEvalTask2Tables = []string{
	"rag_eval_datasets",
	"rag_eval_dataset_versions",
	"rag_eval_corpus_documents",
	"rag_eval_cases",
	"rag_eval_profiles",
	"rag_eval_runs",
	"rag_eval_index_generations",
	"rag_eval_generation_refs",
	"rag_eval_case_results",
	"rag_eval_metric_results",
	"rag_eval_run_aggregates",
	"rag_eval_usage",
}

func TestRAGEvalMigrationSQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "rag-eval-migration.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	runRAGEvalMigrationDialect(t, "sqlite", dsn)
}

func TestRAGEvalMigrationPostgres(t *testing.T) {
	runRAGEvalMigrationDialect(t, "postgres", os.Getenv("BKCRAB_TEST_POSTGRES_DSN"))
}

func TestRAGEvalMigrationMySQL(t *testing.T) {
	runRAGEvalMigrationDialect(t, mysqlDialect, os.Getenv("BKCRAB_TEST_MYSQL_DSN"))
}

func runRAGEvalMigrationDialect(t *testing.T, dialect, dsn string) {
	t.Helper()
	if strings.TrimSpace(dsn) == "" {
		t.Skip("BKCRAB_TEST_" + strings.ToUpper(dialect) + "_DSN is not set")
	}
	st, err := NewDBStore(dialect, dsn)
	if err != nil {
		t.Fatalf("open %s store: %v", dialect, err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("%s initial migration: %v", dialect, err)
	}
	suffix := uuid.NewString()
	userID := "u_eval_migration_" + suffix
	ensureRAGLifecycleUser(t, st, userID, "active")
	defer func() { _ = st.DeleteUser(context.Background(), userID) }()
	kb := &RAGKBRecord{
		ID: "kb_eval_migration_" + suffix, UserID: userID, Name: "migration",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 3,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard, Status: "active",
	}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatalf("%s seed legacy KB: %v", dialect, err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("%s backfill migration attempt %d: %v", dialect, attempt, err)
		}
	}
	generation, documents, err := st.ResolveActiveRAGKBGeneration(ctx, kb.ID)
	if err != nil || generation.CollectionKey != kb.ID || len(documents) != 0 {
		t.Fatalf("%s legacy generation=%+v documents=%+v err=%v", dialect, generation, documents, err)
	}
	for _, table := range ragEvalTask2Tables {
		exists, err := st.tableExists(ctx, table)
		if err != nil {
			t.Fatalf("inspect %s table %s: %v", dialect, table, err)
		}
		if !exists {
			t.Errorf("%s migration did not create %s", dialect, table)
		}
	}
}
