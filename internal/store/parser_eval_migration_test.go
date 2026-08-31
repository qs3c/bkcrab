package store

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestParserEvalMigrationSQLite(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "parser-eval-migration.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	runParserEvalMigrationDialect(t, "sqlite", dsn)
}

func TestParserEvalMigrationPostgres(t *testing.T) {
	runParserEvalMigrationDialect(t, "postgres", os.Getenv("BKCRAB_TEST_POSTGRES_DSN"))
}

func TestParserEvalMigrationMySQL(t *testing.T) {
	runParserEvalMigrationDialect(t, mysqlDialect, os.Getenv("BKCRAB_TEST_MYSQL_DSN"))
}

func runParserEvalMigrationDialect(t *testing.T, dialect, dsn string) {
	t.Helper()
	if strings.TrimSpace(dsn) == "" {
		t.Skip("parser evaluation " + dialect + " DSN is not set")
	}
	st, err := NewDBStore(dialect, dsn)
	if err != nil {
		t.Fatalf("open %s store: %v", dialect, err)
	}
	defer st.Close()
	ctx := context.Background()
	for attempt := 1; attempt <= 2; attempt++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("%s migration attempt %d: %v", dialect, attempt, err)
		}
	}
	for _, table := range parserEvaluationSchemaTables {
		exists, err := st.tableExists(ctx, table)
		if err != nil || !exists {
			t.Fatalf("%s table %s exists=%v err=%v", dialect, table, exists, err)
		}
	}
	if dialect == mysqlDialect {
		for _, table := range parserEvaluationSchemaTables {
			var collation string
			if err := st.DB().QueryRowContext(ctx, `SELECT TABLE_COLLATION FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, table).Scan(&collation); err != nil {
				t.Fatalf("inspect %s collation: %v", table, err)
			}
			if !strings.EqualFold(collation, mysqlRAGSchemaCollation) {
				t.Fatalf("%s collation=%q", table, collation)
			}
		}
	}
}

func TestParserEvalMigrationAddsOnlyTwoDomainTables(t *testing.T) {
	st := openTestDB(t)
	defer st.Close()
	rows, err := st.DB().Query(`SELECT name FROM sqlite_master WHERE type='table' AND name LIKE 'parser_eval_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		got = append(got, name)
	}
	sort.Strings(got)
	want := []string{"parser_eval_documents", "parser_eval_runs"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("parser evaluation tables=%v, want %v", got, want)
	}
	for _, index := range []string{"idx_parser_eval_runs_claim", "idx_parser_eval_runs_expiry", "idx_parser_eval_documents_run"} {
		var name string
		if err := st.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&name); err != nil {
			t.Fatalf("missing index %s: %v", index, err)
		}
	}
}
