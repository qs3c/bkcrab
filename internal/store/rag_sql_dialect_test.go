package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
)

type fakeRAGSQLiteBudgetConn struct {
	commitErr   error
	rollbackErr error
	closeErr    error
	operations  []string
}

func (c *fakeRAGSQLiteBudgetConn) ExecContext(
	_ context.Context,
	query string,
	_ ...any,
) (sql.Result, error) {
	operation := strings.ToUpper(strings.TrimSpace(query))
	c.operations = append(c.operations, operation)
	switch operation {
	case "COMMIT":
		return nil, c.commitErr
	case "ROLLBACK":
		return nil, c.rollbackErr
	default:
		return nil, nil
	}
}

func (c *fakeRAGSQLiteBudgetConn) Close() error {
	c.operations = append(c.operations, "CLOSE")
	return c.closeErr
}

func TestCommitRAGSQLiteBudgetTxRollsBackBeforeCloseOnFailure(t *testing.T) {
	commitErr := errors.New("commit failed")
	rollbackErr := errors.New("rollback failed")
	conn := &fakeRAGSQLiteBudgetConn{commitErr: commitErr, rollbackErr: rollbackErr}

	err := commitRAGSQLiteBudgetTx(conn)
	if !errors.Is(err, commitErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("commit error = %v; want joined commit and rollback failures", err)
	}
	if got := strings.Join(conn.operations, ","); got != "COMMIT,ROLLBACK,CLOSE" {
		t.Fatalf("SQLite transaction cleanup order = %q", got)
	}
}

type fakeRAGReconcileRows struct {
	items        [][2]string
	position     int
	iterationErr error
	closed       bool
}

func (r *fakeRAGReconcileRows) Next() bool {
	if r.position >= len(r.items) {
		return false
	}
	r.position++
	return true
}

func (r *fakeRAGReconcileRows) Scan(dest ...any) error {
	item := r.items[r.position-1]
	*dest[0].(*string) = item[0]
	*dest[1].(*string) = item[1]
	return nil
}

func (r *fakeRAGReconcileRows) Err() error { return r.iterationErr }

func (r *fakeRAGReconcileRows) Close() error {
	r.closed = true
	return nil
}

func TestCollectRAGDocumentAIReconcileCandidatesReturnsIterationError(t *testing.T) {
	iterationErr := errors.New("rows iteration failed")
	rows := &fakeRAGReconcileRows{
		items:        [][2]string{{"usage-1", RAGDocumentAIUsageReserved}},
		iterationErr: iterationErr,
	}

	if _, err := collectRAGDocumentAIReconcileCandidates(rows); !errors.Is(err, iterationErr) {
		t.Fatalf("collect error = %v, want %v", err, iterationErr)
	}
	if !rows.closed {
		t.Fatal("rows were not closed after iteration failure")
	}
}

func TestSQLiteRAGIndexTaskRebuildRestoresStatusIndex(t *testing.T) {
	st := openUnmigratedRAGSQLite(t)
	installLegacyRAGSchema(t, st)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := st.MigrateLegacyRAGIndexTasks(ctx, nil, false); err != nil {
		t.Fatal(err)
	}

	found, unique, columns := ragTaskMigrationIndex(t, st, "rag_index_tasks", "idx_rag_tasks_status")
	if !found || unique || strings.Join(columns, ",") != "status,created_at" {
		t.Fatalf("restored status index = found=%v unique=%v columns=%v", found, unique, columns)
	}
}

func TestEnsureRAGIndexTaskIndexesRejectsWrongNamedSQLiteIndex(t *testing.T) {
	t.Run("column order", func(t *testing.T) {
		st := openTestDB(t)
		defer st.Close()
		ctx := context.Background()
		if _, err := st.db.ExecContext(ctx, `DROP INDEX idx_rag_index_tasks_runnable`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `CREATE INDEX idx_rag_index_tasks_runnable
			ON rag_index_tasks (created_at,status,next_run_at,lease_until)`); err != nil {
			t.Fatal(err)
		}

		err := st.ensureRAGIndexTaskIndexes(ctx)
		if err == nil || !strings.Contains(err.Error(), "incompatible definition") {
			t.Fatalf("wrong named index error = %v", err)
		}
		found, unique, columns := ragTaskMigrationIndex(t, st, "rag_index_tasks", "idx_rag_index_tasks_runnable")
		if !found || unique || strings.Join(columns, ",") != "created_at,status,next_run_at,lease_until" {
			t.Fatalf("unknown index was replaced: found=%v unique=%v columns=%v", found, unique, columns)
		}
	})

	t.Run("uniqueness", func(t *testing.T) {
		st := openTestDB(t)
		defer st.Close()
		ctx := context.Background()
		if err := st.ensureRAGIndexTaskIndexes(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `DROP INDEX uq_rag_index_tasks_doc_version`); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `CREATE INDEX uq_rag_index_tasks_doc_version
			ON rag_index_tasks (doc_id,doc_version)`); err != nil {
			t.Fatal(err)
		}

		err := st.ensureRAGIndexTaskIndexes(ctx)
		if err == nil || !strings.Contains(err.Error(), "incompatible definition") {
			t.Fatalf("non-unique named index error = %v", err)
		}
		found, unique, columns := ragTaskMigrationIndex(t, st, "rag_index_tasks", "uq_rag_index_tasks_doc_version")
		if !found || unique || strings.Join(columns, ",") != "doc_id,doc_version" {
			t.Fatalf("unknown index was replaced: found=%v unique=%v columns=%v", found, unique, columns)
		}
	})
}

func TestMySQLRAGIndexTaskCanonicalDDLRetainsStatusAndRunnableIndexes(t *testing.T) {
	migrationSQL := strings.ToLower(strings.Join(mysqlMigrationSQL(), "\n"))
	for _, expected := range []string{
		"key idx_rag_tasks_status (status, created_at)",
		"key idx_rag_index_tasks_runnable (status, next_run_at, lease_until, created_at)",
	} {
		if !strings.Contains(migrationSQL, expected) {
			t.Errorf("MySQL canonical migration missing %q", expected)
		}
	}
}
