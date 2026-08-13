package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func openRAGFairQueueMySQLTestStore(t *testing.T) *DBStore {
	t.Helper()
	dsn := os.Getenv("BKCRAB_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("BKCRAB_TEST_MYSQL_DSN is not set")
	}
	st, err := NewDBStore(mysqlDialect, dsn)
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestRAGFairQueueCanonicalMySQLDDL(t *testing.T) {
	ddl := strings.ToLower(strings.Join(mysqlMigrationSQL(), "\n"))
	ragTaskDDL := ""
	for _, statement := range mysqlMigrationSQL() {
		if strings.Contains(strings.ToLower(statement), "create table if not exists rag_index_tasks") {
			ragTaskDDL = strings.ToLower(statement)
			break
		}
	}
	if ragTaskDDL == "" {
		t.Fatal("MySQL canonical migration has no rag_index_tasks table")
	}
	for _, token := range []string{
		"user_id varchar(120)",
		"dispatch_generation bigint not null default 1",
		"dispatched_at datetime(6)",
		"key idx_rag_index_tasks_dispatch (status, dispatched_at, next_run_at, id)",
		"key idx_rag_index_tasks_expired (status, lease_until, next_run_at, id)",
		"key idx_rag_index_tasks_user_id (user_id, id)",
		"key idx_rag_index_tasks_user_running (user_id, status, lease_until)",
		"create table if not exists fairqueue_resource_operations",
		"resource varchar(120) character set ascii collate ascii_bin not null",
		"current_writer_fingerprint char(64)",
		"repair_high_water varchar(191)",
		"force_not_before datetime(6)",
	} {
		if !strings.Contains(ddl, token) {
			t.Errorf("MySQL canonical migration is missing %q", token)
		}
	}
	if !strings.Contains(ragTaskDDL, "user_id varchar(120),") ||
		strings.Contains(ragTaskDDL, "user_id varchar(120) not null") {
		t.Fatalf("expand canonical DDL must keep rag_index_tasks.user_id nullable:\n%s", ragTaskDDL)
	}
}

func TestRAGFairQueuePreExpandALTERMySQL(t *testing.T) {
	ctx := context.Background()
	st := openRAGFairQueueMySQLTestStore(t)
	tableName := fmt.Sprintf("rag_index_tasks_expand_%d", time.Now().UTC().UnixNano())
	if !validRAGDDLIdentifier(tableName) {
		t.Fatalf("generated unsafe test table name %q", tableName)
	}
	quotedTable := "`" + tableName + "`"
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(context.Background(), `DROP TABLE IF EXISTS `+quotedTable)
	})

	// Model the last compatible schema before Task 2. The shared production
	// database is never renamed or dropped; every ALTER below targets this
	// uniquely named test-owned table through the same helper as startup.
	if _, err := st.db.ExecContext(ctx, `CREATE TABLE `+quotedTable+` (
		id BIGINT NOT NULL AUTO_INCREMENT,
		doc_id VARCHAR(120) NOT NULL,
		doc_version BIGINT NOT NULL,
		status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
		retry_count INTEGER NOT NULL DEFAULT 0,
		max_retry INTEGER NOT NULL DEFAULT 3,
		claim_generation BIGINT NOT NULL DEFAULT 0,
		lease_owner VARCHAR(96) NOT NULL DEFAULT '',
		lease_until DATETIME(6),
		heartbeat_at DATETIME(6),
		next_run_at DATETIME(6),
		error_msg LONGTEXT NOT NULL,
		created_at DATETIME(6) NOT NULL,
		started_at DATETIME(6),
		finished_at DATETIME(6),
		PRIMARY KEY (id),
		UNIQUE KEY uq_doc_version (doc_id,doc_version)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`); err != nil {
		t.Fatalf("create pre-expand task table: %v", err)
	}
	if err := st.addRAGIndexTaskExpandColumns(ctx, tableName); err != nil {
		t.Fatalf("online expand ALTER: %v", err)
	}
	if err := st.addRAGIndexTaskExpandColumns(ctx, tableName); err != nil {
		t.Fatalf("idempotent online expand ALTER: %v", err)
	}

	for name, want := range map[string]struct {
		nullable string
		dataType string
		def      string
	}{
		"user_id":             {nullable: "YES", dataType: "varchar"},
		"dispatched_at":       {nullable: "YES", dataType: "datetime"},
		"dispatch_generation": {nullable: "NO", dataType: "bigint", def: "1"},
	} {
		var nullable, dataType string
		var defaultValue sql.NullString
		if err := st.db.QueryRowContext(ctx, `SELECT IS_NULLABLE,DATA_TYPE,COLUMN_DEFAULT
			FROM information_schema.columns
			WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, tableName, name).
			Scan(&nullable, &dataType, &defaultValue); err != nil {
			t.Fatalf("inspect expanded %s: %v", name, err)
		}
		if nullable != want.nullable || dataType != want.dataType ||
			(want.def != "" && (!defaultValue.Valid || defaultValue.String != want.def)) {
			t.Errorf("expanded %s nullable=%s type=%s default=%#v; want nullable=%s type=%s default=%q",
				name, nullable, dataType, defaultValue, want.nullable, want.dataType, want.def)
		}
	}

	// The pre-expand writer still omits all three new columns.
	if _, err := st.db.ExecContext(ctx, `INSERT INTO `+quotedTable+`
		(doc_id,doc_version,status,retry_count,max_retry,claim_generation,lease_owner,error_msg,created_at)
		VALUES ('legacy-writer-doc',1,'PENDING',0,3,0,'','',CURRENT_TIMESTAMP(6))`); err != nil {
		t.Fatalf("legacy INSERT after online ALTER: %v", err)
	}
	var userID sql.NullString
	var dispatchedAt sql.NullTime
	var dispatchGeneration int64
	if err := st.db.QueryRowContext(ctx, `SELECT user_id,dispatched_at,dispatch_generation FROM `+quotedTable+`
		WHERE doc_id='legacy-writer-doc'`).Scan(&userID, &dispatchedAt, &dispatchGeneration); err != nil {
		t.Fatal(err)
	}
	if userID.Valid || dispatchedAt.Valid || dispatchGeneration != 1 {
		t.Fatalf("legacy defaults after ALTER user=%#v dispatched=%#v generation=%d",
			userID, dispatchedAt, dispatchGeneration)
	}
}

func TestRAGFairQueueExpandAndBackfillMySQL(t *testing.T) {
	ctx := context.Background()
	st := openRAGFairQueueMySQLTestStore(t)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("expand migration: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("idempotent expand migration: %v", err)
	}

	type columnExpectation struct {
		nullable bool
		dataType string
		def      string
	}
	for name, want := range map[string]columnExpectation{
		"user_id":             {nullable: true, dataType: "varchar"},
		"dispatched_at":       {nullable: true, dataType: "datetime"},
		"dispatch_generation": {nullable: false, dataType: "bigint", def: "1"},
	} {
		var nullable, dataType string
		var defaultValue sql.NullString
		if err := st.db.QueryRowContext(ctx, `SELECT IS_NULLABLE,DATA_TYPE,COLUMN_DEFAULT
			FROM information_schema.columns
			WHERE table_schema=DATABASE() AND table_name='rag_index_tasks' AND column_name=?`, name).
			Scan(&nullable, &dataType, &defaultValue); err != nil {
			t.Fatalf("inspect rag_index_tasks.%s: %v", name, err)
		}
		if got := nullable == "YES"; got != want.nullable {
			t.Errorf("rag_index_tasks.%s nullable = %v, want %v", name, got, want.nullable)
		}
		if dataType != want.dataType {
			t.Errorf("rag_index_tasks.%s type = %q, want %q", name, dataType, want.dataType)
		}
		if want.def != "" && (!defaultValue.Valid || defaultValue.String != want.def) {
			t.Errorf("rag_index_tasks.%s default = %#v, want %q", name, defaultValue, want.def)
		}
	}

	for name, columns := range map[string]string{
		"idx_rag_index_tasks_dispatch":     "status,dispatched_at,next_run_at,id",
		"idx_rag_index_tasks_expired":      "status,lease_until,next_run_at,id",
		"idx_rag_index_tasks_user_id":      "user_id,id",
		"idx_rag_index_tasks_user_running": "user_id,status,lease_until",
	} {
		var count, maxNonUnique int
		var got string
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(MAX(NON_UNIQUE),0),
			COALESCE(GROUP_CONCAT(COLUMN_NAME ORDER BY SEQ_IN_INDEX SEPARATOR ','),'')
			FROM information_schema.statistics
			WHERE table_schema=DATABASE() AND table_name='rag_index_tasks' AND index_name=?`, name).
			Scan(&count, &maxNonUnique, &got); err != nil {
			t.Fatalf("inspect index %s: %v", name, err)
		}
		if count == 0 || maxNonUnique != 1 || got != columns {
			t.Errorf("index %s = count=%d nonUnique=%d columns=%q, want non-unique %q", name, count, maxNonUnique, got, columns)
		}
	}

	suffix := fmt.Sprintf("fq_%d", time.Now().UTC().UnixNano())
	userID, kbID := "u_"+suffix, "kb_"+suffix
	docIDs := []string{"pending_" + suffix, "running_" + suffix, "done_" + suffix, "legacy_" + suffix}
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(context.Background(), `DELETE FROM rag_index_tasks WHERE doc_id IN (?,?,?,?)`,
			docIDs[0], docIDs[1], docIDs[2], docIDs[3])
		_, _ = st.db.ExecContext(context.Background(), `DELETE FROM rag_documents WHERE kb_id=?`, kbID)
		_, _ = st.db.ExecContext(context.Background(), `DELETE FROM rag_kbs WHERE id=?`, kbID)
	})

	now := time.Now().UTC()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_kbs
		(id,user_id,name,description,embed_model,embed_dims,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?)`, kbID, userID, suffix, "", "test", 3, now, now); err != nil {
		t.Fatalf("insert KB: %v", err)
	}
	for _, docID := range docIDs {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_documents
			(id,kb_id,file_name,file_type,object_key,error_msg,uploaded_at)
			VALUES (?,?,?,?,?,?,?)`, docID, kbID, docID+".txt", "txt", "test/"+docID, "", now); err != nil {
			t.Fatalf("insert document %s: %v", docID, err)
		}
	}

	// This is the compatibility writer shape from before the expand release: all
	// new columns are intentionally omitted.
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_index_tasks
		(doc_id,doc_version,status,retry_count,max_retry,claim_generation,lease_owner,error_msg,created_at)
		VALUES (?,1,'PENDING',0,3,0,'','',?)`, docIDs[3], now); err != nil {
		t.Fatalf("legacy INSERT after expand: %v", err)
	}
	var legacyUser sql.NullString
	var legacyDispatched sql.NullTime
	var legacyDispatchGeneration int64
	if err := st.db.QueryRowContext(ctx, `SELECT user_id,dispatched_at,dispatch_generation
		FROM rag_index_tasks WHERE doc_id=? AND doc_version=1`, docIDs[3]).
		Scan(&legacyUser, &legacyDispatched, &legacyDispatchGeneration); err != nil {
		t.Fatalf("read legacy task: %v", err)
	}
	if legacyUser.Valid || legacyDispatched.Valid || legacyDispatchGeneration != 1 {
		t.Fatalf("legacy defaults = user=%#v dispatched=%#v generation=%d", legacyUser, legacyDispatched, legacyDispatchGeneration)
	}

	for _, seed := range []struct {
		docID              string
		status             string
		dispatchGeneration int64
		claimGeneration    int64
	}{
		{docID: docIDs[0], status: "PENDING", dispatchGeneration: 2, claimGeneration: 4},
		{docID: docIDs[1], status: "RUNNING", dispatchGeneration: 1, claimGeneration: 3},
		{docID: docIDs[2], status: "DONE", dispatchGeneration: 9, claimGeneration: 2},
	} {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_index_tasks
			(doc_id,doc_version,status,retry_count,max_retry,dispatch_generation,claim_generation,
			 dispatched_at,lease_owner,error_msg,created_at)
			VALUES (?,1,?,0,3,?,?,?,'','',?)`, seed.docID, seed.status,
			seed.dispatchGeneration, seed.claimGeneration, now, now); err != nil {
			t.Fatalf("seed %s task: %v", seed.status, err)
		}
	}

	afterID := int64(0)
	for page := 0; page < 10000; page++ {
		nextID, _, done, err := st.backfillRAGFairQueueTasksPage(ctx, st.db, afterID, 2)
		if err != nil {
			t.Fatalf("backfill page after %d: %v", afterID, err)
		}
		if done {
			break
		}
		if nextID <= afterID {
			t.Fatalf("backfill cursor did not advance: after=%d next=%d", afterID, nextID)
		}
		afterID = nextID
		if page == 9999 {
			t.Fatal("backfill did not terminate")
		}
	}

	for _, want := range []struct {
		docID              string
		dispatchGeneration int64
		dispatched         bool
	}{
		{docID: docIDs[0], dispatchGeneration: 5, dispatched: false},
		{docID: docIDs[1], dispatchGeneration: 3, dispatched: true},
		{docID: docIDs[2], dispatchGeneration: 9, dispatched: true},
		{docID: docIDs[3], dispatchGeneration: 1, dispatched: false},
	} {
		var gotUser sql.NullString
		var gotDispatch int64
		var gotMarker sql.NullTime
		if err := st.db.QueryRowContext(ctx, `SELECT user_id,dispatch_generation,dispatched_at
			FROM rag_index_tasks WHERE doc_id=? AND doc_version=1`, want.docID).
			Scan(&gotUser, &gotDispatch, &gotMarker); err != nil {
			t.Fatalf("read backfilled task %s: %v", want.docID, err)
		}
		if !gotUser.Valid || gotUser.String != userID || gotDispatch != want.dispatchGeneration || gotMarker.Valid != want.dispatched {
			t.Errorf("task %s after backfill = user=%#v generation=%d dispatched=%v", want.docID, gotUser, gotDispatch, gotMarker.Valid)
		}
	}
}

func TestRAGFairQueueOperationJournalMySQL(t *testing.T) {
	ctx := context.Background()
	st := openRAGFairQueueMySQLTestStore(t)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migration: %v", err)
	}
	identity, err := st.discoverFairQueueWriterIdentity(ctx)
	if err != nil {
		t.Fatalf("discover writer identity: %v", err)
	}
	admin := &FairQueueAdminStore{db: st, writerFingerprint: identity.fingerprint}
	report, err := admin.CheckRAGFairQueueContract(ctx)
	if err != nil || !report.ExpandSchemaReady || !report.UserIDNullable || report.Contracted {
		t.Fatalf("expand contract report=%+v err=%v", report, err)
	}

	suffix := fmt.Sprintf("%d", time.Now().UTC().UnixNano())
	resource := "test.rag.journal." + suffix
	lockedResource := "test.rag.lock." + suffix
	t.Cleanup(func() {
		_, _ = st.db.ExecContext(context.Background(),
			`DELETE FROM fairqueue_resource_operations WHERE resource IN (?,?)`, resource, lockedResource)
	})

	operationID, err := NewFairQueueOperationID()
	if err != nil {
		t.Fatal(err)
	}
	var active FairQueueOperationRecord
	err = st.WithFairQueueOperationStartFence(ctx, resource, identity.fingerprint,
		func(session *FairQueueOperationStartSession) error {
			if _, found, readErr := session.Read(ctx); readErr != nil || found {
				return fmt.Errorf("initial journal read found=%v: %w", found, readErr)
			}
			var beginErr error
			active, beginErr = session.BeginSpecial(ctx, nil, FairQueueOperationProposal{
				Resource: resource, OperationID: operationID,
				Kind: FairQueueOperationRabbitRepair, CurrentWriterFingerprint: identity.fingerprint,
			})
			return beginErr
		})
	if err != nil {
		t.Fatalf("begin journal operation under MySQL start fence: %v", err)
	}
	reopened, err := NewDBStore(mysqlDialect, os.Getenv("BKCRAB_TEST_MYSQL_DSN"))
	if err != nil {
		t.Fatalf("reopen MySQL journal store: %v", err)
	}
	reopenedActive, reopenedFound, reopenErr := reopened.ReadFairQueueOperation(
		ctx, resource, identity.fingerprint)
	closeErr := reopened.Close()
	if reopenErr != nil || !reopenedFound || !fairQueueOperationCASMatches(active, reopenedActive) || closeErr != nil {
		t.Fatalf("reopened ACTIVE=%+v found=%v readErr=%v closeErr=%v", reopenedActive, reopenedFound, reopenErr, closeErr)
	}
	withHighWater, err := st.SetFairQueueOperationRepairHighWater(ctx, active, "123")
	if err != nil {
		t.Fatalf("set repair high water: %v", err)
	}
	passComplete, err := st.MarkFairQueueOperationRepairPassComplete(ctx, withHighWater)
	if err != nil {
		t.Fatalf("mark repair pass: %v", err)
	}
	ready, err := st.CommitFairQueueOperationReady(ctx, passComplete)
	if err != nil {
		t.Fatalf("commit ready: %v", err)
	}
	completed, err := st.CompleteFairQueueOperation(ctx, ready)
	if err != nil {
		t.Fatalf("complete operation: %v", err)
	}
	persisted, found, err := st.ReadFairQueueOperation(ctx, resource, identity.fingerprint)
	if err != nil || !found || !fairQueueOperationCASMatches(completed, persisted) {
		t.Fatalf("persisted operation=%+v found=%v err=%v", persisted, found, err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- st.WithFairQueueOperationStartFence(context.Background(), lockedResource,
			identity.fingerprint, func(*FairQueueOperationStartSession) error {
				close(entered)
				<-release
				return nil
			})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("first MySQL start fence did not enter")
	}
	waitCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	secondErr := st.WithFairQueueOperationStartFence(waitCtx, lockedResource,
		identity.fingerprint, func(*FairQueueOperationStartSession) error {
			return errors.New("competing callback must not run")
		})
	cancel()
	close(release)
	firstErr := <-firstDone
	if firstErr != nil {
		t.Fatalf("first MySQL start fence: %v", firstErr)
	}
	if !errors.Is(secondErr, ErrFairQueueStartLockUnavailable) {
		t.Fatalf("competing MySQL start fence error=%v, want lock unavailable", secondErr)
	}
}

func TestRAGFairQueueContractApplyMySQL(t *testing.T) {
	ctx := context.Background()
	st := openRAGFairQueueMySQLTestStore(t)
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("expand migration: %v", err)
	}
	var existingTasks int64
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_index_tasks`).Scan(&existingTasks); err != nil {
		t.Fatalf("count pre-existing contract tasks: %v", err)
	}
	if existingTasks != 0 {
		t.Skipf("contract Apply integration requires a dedicated empty schema; found %d existing tasks", existingTasks)
	}

	suffix := fmt.Sprintf("fq_contract_%d", time.Now().UTC().UnixNano())
	userID, kbID, docID := "u_"+suffix, "kb_"+suffix, "doc_"+suffix
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := st.db.ExecContext(cleanupCtx,
			`ALTER TABLE rag_index_tasks MODIFY COLUMN user_id VARCHAR(120) NULL`); err != nil {
			t.Errorf("restore nullable contract column: %v", err)
		}
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_index_tasks WHERE doc_id=?`, docID)
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_documents WHERE id=?`, docID)
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_kbs WHERE id=?`, kbID)
		_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM users WHERE id=?`, userID)
	})
	if _, err := st.db.ExecContext(ctx,
		`ALTER TABLE rag_index_tasks MODIFY COLUMN user_id VARCHAR(120) NULL`); err != nil {
		t.Fatalf("prepare nullable contract column: %v", err)
	}
	now := time.Now().UTC()
	if _, err := st.db.ExecContext(ctx, `INSERT INTO users
		(id,username,email,avatar_url,status) VALUES (?,?,?,?,'active')`,
		userID, userID, userID+"@example.invalid", ""); err != nil {
		t.Fatalf("insert contract user: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_kbs
		(id,user_id,name,description,embed_model,embed_dims,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?)`, kbID, userID, suffix, "", "test", 3, now, now); err != nil {
		t.Fatalf("insert contract KB: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_documents
		(id,kb_id,file_name,file_type,object_key,error_msg,uploaded_at)
		VALUES (?,?,?,?,?,?,?)`, docID, kbID, docID+".txt", "txt", "test/"+docID, "", now); err != nil {
		t.Fatalf("insert contract document: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_index_tasks
		(doc_id,doc_version,status,retry_count,max_retry,dispatch_generation,claim_generation,
		 dispatched_at,lease_owner,error_msg,created_at)
		VALUES (?,1,'PENDING',0,3,1,3,?,'','',?)`, docID, now, now); err != nil {
		t.Fatalf("insert legacy contract task: %v", err)
	}

	admin, err := OpenFairQueueAdminStore(StorageConfig{
		Type: StorageMySQL, DSN: os.Getenv("BKCRAB_TEST_MYSQL_DSN"), AutoMigrate: false,
	})
	if err != nil {
		t.Fatalf("open non-migrating admin store: %v", err)
	}
	defer admin.Close()
	before, err := admin.CheckRAGFairQueueContract(ctx)
	if err != nil || !before.ExpandSchemaReady || !before.UserIDNullable || before.RemainingCount == 0 {
		t.Fatalf("pre-contract report=%+v err=%v", before, err)
	}
	applied, err := admin.ApplyRAGFairQueueContract(ctx, RAGFairQueueContractAttestation{AllWritersDualWrite: true})
	if err != nil || !applied.Contracted || applied.UserIDNullable || applied.RowsChanged == 0 || applied.PagesScanned == 0 {
		t.Fatalf("applied contract report=%+v err=%v", applied, err)
	}

	var gotUser string
	var gotDispatch int64
	var gotMarker sql.NullTime
	if err := st.db.QueryRowContext(ctx, `SELECT user_id,dispatch_generation,dispatched_at
		FROM rag_index_tasks WHERE doc_id=?`, docID).Scan(&gotUser, &gotDispatch, &gotMarker); err != nil {
		t.Fatalf("read contracted task: %v", err)
	}
	if gotUser != userID || gotDispatch != 4 || gotMarker.Valid {
		t.Fatalf("contracted task user=%q dispatch=%d marker=%#v", gotUser, gotDispatch, gotMarker)
	}
	var nullable string
	if err := st.db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
		WHERE table_schema=DATABASE() AND table_name='rag_index_tasks' AND column_name='user_id'`).Scan(&nullable); err != nil {
		t.Fatalf("inspect contracted user_id: %v", err)
	}
	if nullable != "NO" {
		t.Fatalf("contracted user_id nullable=%q", nullable)
	}

	repeated, err := admin.ApplyRAGFairQueueContract(ctx, RAGFairQueueContractAttestation{AllWritersDualWrite: true})
	if err != nil || !repeated.Contracted || repeated.RowsChanged != 0 {
		t.Fatalf("idempotent contract report=%+v err=%v", repeated, err)
	}
	reopened, err := OpenFairQueueAdminStore(StorageConfig{
		Type: StorageMySQL, DSN: os.Getenv("BKCRAB_TEST_MYSQL_DSN"), AutoMigrate: false,
	})
	if err != nil {
		t.Fatalf("reopen contracted admin store: %v", err)
	}
	reopenedReport, reopenErr := reopened.CheckRAGFairQueueContract(ctx)
	closeErr := reopened.Close()
	if reopenErr != nil || !reopenedReport.Contracted || closeErr != nil {
		t.Fatalf("reopened contract report=%+v readErr=%v closeErr=%v", reopenedReport, reopenErr, closeErr)
	}
}

type ragFairQueueMySQLClaimTask struct {
	taskID int64
	docID  string
	kbID   string
	userID string
}

func seedRAGFairQueueMySQLClaimTasks(
	t *testing.T,
	st *DBStore,
	suffix string,
	users []string,
) []ragFairQueueMySQLClaimTask {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	tasks := make([]ragFairQueueMySQLClaimTask, 0, len(users))
	for i, userID := range users {
		ensureRAGLifecycleUser(t, st, userID, "active")
		kbID := fmt.Sprintf("kb_%s_%d", suffix, i)
		docID := fmt.Sprintf("doc_%s_%d", suffix, i)
		if err := st.CreateRAGKB(ctx, &RAGKBRecord{
			ID: kbID, UserID: userID, Name: kbID, EmbedProvider: "system",
			EmbedModel: "embed-v1", EmbedDims: 3, ChunkSize: 512, ChunkOverlap: 64,
			ParseMode: RAGParseModeStandard, Status: "active", CreatedAt: now,
		}); err != nil {
			t.Fatalf("create fair claim KB %s: %v", kbID, err)
		}
		doc := &RAGDocumentRecord{
			ID: docID, KBID: kbID, FileName: docID + ".md", FileType: "md", FileSize: 1,
			ObjectKey: "rag/" + userID + "/" + kbID + "/" + docID + ".md",
			Status:    "PENDING", Version: 1, SourceSHA256: testRAGVersion(docID, 1).SourceSHA256,
			IndexFormatVersion: 1, ProcessingStage: "queued", UploadedAt: now,
		}
		taskID, err := st.CreateRAGDocumentWithVersionAndIndexTask(
			ctx, doc, testRAGVersion(docID, 1), 3,
		)
		if err != nil {
			t.Fatalf("create fair claim task %s: %v", docID, err)
		}
		tasks = append(tasks, ragFairQueueMySQLClaimTask{
			taskID: taskID, docID: docID, kbID: kbID, userID: userID,
		})
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for i := len(tasks) - 1; i >= 0; i-- {
			task := tasks[i]
			_, _ = st.db.ExecContext(cleanupCtx,
				`DELETE FROM rag_document_ai_usage WHERE task_id=?`, task.taskID)
			_, _ = st.db.ExecContext(cleanupCtx,
				`DELETE FROM rag_document_ai_task_budgets WHERE task_id=?`, task.taskID)
			_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_index_tasks WHERE id=?`, task.taskID)
			_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_document_versions WHERE doc_id=?`, task.docID)
			_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_documents WHERE id=?`, task.docID)
			_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM rag_kbs WHERE id=?`, task.kbID)
			_, _ = st.db.ExecContext(cleanupCtx, `DELETE FROM users WHERE id=?`, task.userID)
		}
	})
	return tasks
}

func TestRAGFairQueueExactClaimMySQLCapacityIsGlobalAcrossStores(t *testing.T) {
	ctx := context.Background()
	primary := openRAGFairQueueMySQLTestStore(t)
	if err := primary.Migrate(ctx); err != nil {
		t.Fatalf("migrate exact claim schema: %v", err)
	}
	identity, err := primary.ReadFairQueueWriterIdentity(ctx)
	if err != nil {
		t.Fatalf("read exact claim writer: %v", err)
	}
	first, err := primary.BindRAGFairQueueWriter(identity.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	secondStore, err := NewDBStore(mysqlDialect, os.Getenv("BKCRAB_TEST_MYSQL_DSN"))
	if err != nil {
		t.Fatalf("open second exact claim store: %v", err)
	}
	t.Cleanup(func() { _ = secondStore.Close() })
	second, err := secondStore.BindRAGFairQueueWriter(identity.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}

	highWater, err := first.CaptureRAGFairQueueHighWater(ctx)
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := first.ListValidRunningRAGIndexTasksPage(ctx, highWater, 0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 {
		t.Skipf("exact capacity integration requires no pre-existing valid RUNNING tasks; found %d", len(running))
	}

	tests := []struct {
		name   string
		global int
		burst  int
		users  func(string) []string
	}{
		{
			name: "global", global: 4, burst: 4,
			users: func(suffix string) []string {
				users := make([]string, 8)
				for i := range users {
					users[i] = fmt.Sprintf("u_%s_%d", suffix, i)
				}
				return users
			},
		},
		{
			name: "per-user-burst", global: 8, burst: 4,
			users: func(suffix string) []string {
				users := make([]string, 8)
				for i := range users {
					users[i] = "u_" + suffix
				}
				return users
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			suffix := fmt.Sprintf("fq_claim_%s_%d", strings.ReplaceAll(test.name, "-", "_"), time.Now().UnixNano())
			tasks := seedRAGFairQueueMySQLClaimTasks(t, primary, suffix, test.users(suffix))
			type outcome struct {
				result RAGFairQueueClaimResult
				err    error
			}
			start := make(chan struct{})
			outcomes := make(chan outcome, len(tasks))
			var wg sync.WaitGroup
			for i, task := range tasks {
				wg.Add(1)
				go func(i int, task ragFairQueueMySQLClaimTask) {
					defer wg.Done()
					<-start
					facade := first
					if i%2 == 1 {
						facade = second
					}
					result, err := facade.ClaimRAGIndexTaskByID(ctx, task.taskID, task.userID, 1,
						fmt.Sprintf("fair-mysql-worker-%d", i), time.Minute, RAGFairQueueClaimLimits{
							GlobalConcurrency: test.global, PerUserBurstConcurrency: test.burst,
						})
					outcomes <- outcome{result: result, err: err}
				}(i, task)
			}
			close(start)
			wg.Wait()
			close(outcomes)
			claimed, deferred := 0, 0
			for outcome := range outcomes {
				if outcome.err != nil {
					t.Fatalf("concurrent exact claim: %v", outcome.err)
				}
				switch outcome.result.Disposition {
				case RAGFairQueueClaimed:
					claimed++
				case RAGFairQueueClaimCapacityDeferred:
					deferred++
				default:
					t.Fatalf("unexpected exact claim disposition %q", outcome.result.Disposition)
				}
			}
			if claimed != 4 || deferred != 4 {
				t.Fatalf("claimed=%d deferred=%d, want 4/4", claimed, deferred)
			}
			validRunning := 0
			for _, task := range tasks {
				row, err := primary.GetRAGIndexTask(ctx, task.taskID)
				if err != nil {
					t.Fatal(err)
				}
				if row.Status == "RUNNING" && row.DispatchGeneration == row.ClaimGeneration {
					validRunning++
				}
			}
			if validRunning != 4 {
				t.Fatalf("valid RUNNING=%d, want 4", validRunning)
			}
		})
	}
}

func TestRAGFairQueueExactClaimMySQLAdvisoryLockTimeoutDoesNotMutate(t *testing.T) {
	ctx := context.Background()
	st := openRAGFairQueueMySQLTestStore(t)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	identity, err := st.discoverFairQueueWriterIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facade, err := st.BindRAGFairQueueWriter(identity.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("fq_claim_lock_%d", time.Now().UnixNano())
	userID := "u_" + suffix
	tasks := seedRAGFairQueueMySQLClaimTasks(t, st, suffix, []string{userID})

	st.db.SetMaxOpenConns(4)
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	lockName := fairQueueCapacityLockName(identity.database, "rag.index")
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 1)`, lockName).Scan(&acquired); err != nil ||
		!acquired.Valid || acquired.Int64 != 1 {
		t.Fatalf("hold exact claim capacity lock: acquired=%v err=%v", acquired, err)
	}
	defer func() {
		var released sql.NullInt64
		_ = conn.QueryRowContext(context.Background(), `SELECT RELEASE_LOCK(?)`, lockName).Scan(&released)
	}()

	result, err := facade.ClaimRAGIndexTaskByID(ctx, tasks[0].taskID, userID, 1,
		"fair-lock-timeout-worker", time.Minute, RAGFairQueueClaimLimits{
			GlobalConcurrency: 4, PerUserBurstConcurrency: 4, AdvisoryLockTimeout: 100 * time.Millisecond,
		})
	if !errors.Is(err, ErrRAGFairQueueCapacityLockUnavailable) || result.Claim != nil {
		t.Fatalf("lock-timeout result=%+v err=%v", result, err)
	}
	task, readErr := st.GetRAGIndexTask(ctx, tasks[0].taskID)
	if readErr != nil || task.Status != "PENDING" || task.DispatchGeneration != 1 ||
		task.ClaimGeneration != 0 || task.DispatchedAt != nil {
		t.Fatalf("lock-timeout mutated task=%+v err=%v", task, readErr)
	}
}

func TestRAGFairQueueHeartbeatAndExpiredRearmShareMySQLCapacityFence(t *testing.T) {
	ctx := context.Background()
	st := openRAGFairQueueMySQLTestStore(t)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	identity, err := st.discoverFairQueueWriterIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	facade, err := st.BindRAGFairQueueWriter(identity.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("fq_heartbeat_sweeper_%d", time.Now().UnixNano())
	users := []string{"u_" + suffix + "_heartbeat", "u_" + suffix + "_sweeper"}
	tasks := seedRAGFairQueueMySQLClaimTasks(t, st, suffix, users)
	limits := RAGFairQueueClaimLimits{GlobalConcurrency: 128, PerUserBurstConcurrency: 128}

	heartbeatClaim, err := facade.ClaimRAGIndexTaskByID(
		ctx, tasks[0].taskID, tasks[0].userID, 1, "fair-heartbeat-winner", time.Minute, limits,
	)
	if err != nil || heartbeatClaim.Disposition != RAGFairQueueClaimed || heartbeatClaim.Claim == nil {
		t.Fatalf("claim heartbeat task: result=%+v err=%v", heartbeatClaim, err)
	}
	if renewed, err := st.HeartbeatRAGIndexTask(
		ctx, heartbeatClaim.Claim.Fence, 2*time.Minute,
	); err != nil || !renewed {
		t.Fatalf("renew heartbeat task: renewed=%v err=%v", renewed, err)
	}
	page, _, err := facade.ArmExpiredRAGIndexTasksPage(ctx, 0, 10_000)
	if err != nil {
		t.Fatalf("arm after heartbeat: %v", err)
	}
	for _, candidate := range page {
		if candidate.Task.ID == tasks[0].taskID {
			t.Fatalf("renewed heartbeat task was rearmed: %+v", candidate)
		}
	}

	sweeperClaim, err := facade.ClaimRAGIndexTaskByID(
		ctx, tasks[1].taskID, tasks[1].userID, 1, "fair-sweeper-winner", time.Minute, limits,
	)
	if err != nil || sweeperClaim.Disposition != RAGFairQueueClaimed || sweeperClaim.Claim == nil {
		t.Fatalf("claim sweeper task: result=%+v err=%v", sweeperClaim, err)
	}

	st.db.SetMaxOpenConns(6)
	lockConn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Close()
	lockName := fairQueueCapacityLockName(identity.database, "rag.index")
	var acquired sql.NullInt64
	if err := lockConn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 1)`, lockName).Scan(&acquired); err != nil ||
		!acquired.Valid || acquired.Int64 != 1 {
		t.Fatalf("hold heartbeat/sweeper capacity lock: acquired=%v err=%v", acquired, err)
	}
	released := false
	defer func() {
		if released {
			return
		}
		var result sql.NullInt64
		_ = lockConn.QueryRowContext(context.Background(), `SELECT RELEASE_LOCK(?)`, lockName).Scan(&result)
	}()

	heartbeatCtx, cancelHeartbeat := context.WithTimeout(ctx, 150*time.Millisecond)
	renewed, heartbeatErr := st.HeartbeatRAGIndexTask(
		heartbeatCtx, sweeperClaim.Claim.Fence, 2*time.Minute,
	)
	cancelHeartbeat()
	if renewed || !errors.Is(heartbeatErr, ErrRAGFairQueueCapacityLockUnavailable) {
		t.Fatalf("heartbeat crossed held capacity lock: renewed=%v err=%v", renewed, heartbeatErr)
	}
	if _, err := lockConn.ExecContext(ctx,
		`UPDATE rag_index_tasks SET lease_until=DATE_SUB(UTC_TIMESTAMP(6), INTERVAL 1 SECOND)
		 WHERE id=?`, tasks[1].taskID); err != nil {
		t.Fatalf("expire sweeper task: %v", err)
	}
	sweeperCtx, cancelSweeper := context.WithTimeout(ctx, 150*time.Millisecond)
	blockedPage, _, sweeperErr := facade.ArmExpiredRAGIndexTasksPage(sweeperCtx, 0, 10_000)
	cancelSweeper()
	if !errors.Is(sweeperErr, ErrRAGFairQueueCapacityLockUnavailable) || len(blockedPage) != 0 {
		t.Fatalf("sweeper crossed held capacity lock: page=%+v err=%v", blockedPage, sweeperErr)
	}
	before, err := st.GetRAGIndexTask(ctx, tasks[1].taskID)
	if err != nil || before.Status != "RUNNING" ||
		before.DispatchGeneration != before.ClaimGeneration {
		t.Fatalf("blocked sweeper mutated task=%+v err=%v", before, err)
	}

	var releaseResult sql.NullInt64
	if err := lockConn.QueryRowContext(ctx, `SELECT RELEASE_LOCK(?)`, lockName).Scan(&releaseResult); err != nil ||
		!releaseResult.Valid || releaseResult.Int64 != 1 {
		t.Fatalf("release heartbeat/sweeper capacity lock: result=%v err=%v", releaseResult, err)
	}
	released = true

	rearmed, _, err := facade.ArmExpiredRAGIndexTasksPage(ctx, 0, 10_000)
	if err != nil {
		t.Fatalf("rearm expired task: %v", err)
	}
	var found *RAGIndexTaskDispatchCandidate
	for i := range rearmed {
		if rearmed[i].Task.ID == tasks[1].taskID {
			found = &rearmed[i]
			break
		}
	}
	if found == nil || found.Task.DispatchGeneration != sweeperClaim.Claim.Fence.ClaimGeneration+1 {
		t.Fatalf("expired task rearm=%+v", found)
	}
	if renewed, err := st.HeartbeatRAGIndexTask(
		ctx, sweeperClaim.Claim.Fence, 2*time.Minute,
	); err != nil || renewed {
		t.Fatalf("old heartbeat revived swept task: renewed=%v err=%v", renewed, err)
	}
}

func TestRAGFairQueueCapacityFenceMySQLReleaseFailureFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := openRAGFairQueueMySQLTestStore(t)
	st.db.SetMaxOpenConns(4)
	identity, err := st.discoverFairQueueWriterIdentity(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var killErr error
	err = st.withRAGFairQueueCapacityLock(
		ctx, identity.fingerprint, time.Second,
		func(session ragFairQueueCapacitySession) error {
			_, killErr = st.db.ExecContext(ctx, fmt.Sprintf(
				"KILL CONNECTION %d", session.identity.connectionID,
			))
			return nil
		},
	)
	if killErr != nil {
		t.Skipf("MySQL test principal cannot kill its own capacity session: %v", killErr)
	}
	if !errors.Is(err, ErrFairQueueUnsafeConnection) {
		t.Fatalf("killed capacity session error=%v", err)
	}
	lockName := fairQueueCapacityLockName(identity.database, "rag.index")
	deadline := time.Now().Add(5 * time.Second)
	for {
		var owner sql.NullInt64
		if queryErr := st.db.QueryRowContext(ctx, `SELECT IS_USED_LOCK(?)`, lockName).Scan(&owner); queryErr != nil {
			t.Fatalf("inspect killed capacity lock: %v", queryErr)
		}
		if !owner.Valid {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("killed capacity session leaked named lock to connection %d", owner.Int64)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if snapshot := st.ReadFairQueueConnectionSafetySnapshot(); snapshot.SessionAffinity != FairQueueSessionAffinityMismatch {
		t.Fatalf("release failure safety snapshot=%+v", snapshot)
	}
}
