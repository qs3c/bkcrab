package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

type ragFairQueueAdminSourceContract interface {
	ReadWriterIdentity(context.Context) (FairQueueWriterIdentity, error)
	CheckSchemaAndInvariants(context.Context) (RAGFairQueueContractReport, error)
	CountValidRunning(context.Context) (int64, error)

	CaptureRAGFairQueueHighWater(context.Context) (int64, error)
	ListCanonicalRAGTenantsPage(context.Context, int64, string, int) ([]string, string, error)
	ListDispatchedRAGIndexTasksPage(context.Context, int64, int64, int) ([]RAGIndexTaskRecord, int64, error)
	ListValidRunningRAGIndexTasksPage(context.Context, int64, int64, int) ([]RAGIndexTaskRunningSnapshot, int64, error)

	ReadFairQueueOperation(context.Context, string, string) (FairQueueOperationRecord, bool, error)
	WithFairQueueOperationStartFence(context.Context, string, string, func(*FairQueueOperationStartSession) error) error
	SetFairQueueOperationRepairHighWater(context.Context, FairQueueOperationRecord, string) (FairQueueOperationRecord, error)
	MarkFairQueueOperationRepairPassComplete(context.Context, FairQueueOperationRecord) (FairQueueOperationRecord, error)
	MarkFairQueueOperationForceDeletePassComplete(context.Context, FairQueueOperationRecord) (FairQueueOperationRecord, error)
	CommitFairQueueOperationReady(context.Context, FairQueueOperationRecord) (FairQueueOperationRecord, error)
	CompleteFairQueueOperation(context.Context, FairQueueOperationRecord) (FairQueueOperationRecord, error)
}

var _ ragFairQueueAdminSourceContract = (*RAGFairQueueAdminSource)(nil)

func TestRAGFairQueueAdminStoreRejectsUnsafeOpenModes(t *testing.T) {
	tests := []struct {
		name string
		cfg  StorageConfig
		want error
	}{
		{
			name: "non mysql",
			cfg:  StorageConfig{Type: StorageSQLite, DSN: ":memory:"},
			want: ErrFairQueueMySQLRequired,
		},
		{
			name: "auto migrate",
			cfg: StorageConfig{
				Type: StorageMySQL, DSN: "ignored", AutoMigrate: true,
			},
		},
		{
			name: "missing dsn",
			cfg:  StorageConfig{Type: StorageMySQL, AutoMigrate: false},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			opened, err := OpenFairQueueAdminStore(test.cfg)
			if opened != nil || err == nil {
				t.Fatalf("opened=%v err=%v", opened, err)
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("error=%v want %v", err, test.want)
			}
		})
	}
}

func TestRAGFairQueueAdminSourceReadsFreshBoundWriterIdentity(t *testing.T) {
	state := &fairQueueFenceDriverState{connIDs: []int64{41, 41}}
	db, expected := newFairQueueFenceTestStore(t, state)
	admin := &FairQueueAdminStore{
		db: db,
		// The construction-time value is intentionally wrong: the source must
		// re-read the pinned physical writer rather than echo this cache.
		writerFingerprint: strings.Repeat("f", 64),
	}
	source, err := admin.BindRAGFairQueueSource(expected)
	if err != nil {
		t.Fatalf("bind admin source: %v", err)
	}
	identity, err := source.ReadWriterIdentity(context.Background())
	if err != nil || identity.Fingerprint != expected {
		t.Fatalf("fresh identity=%+v err=%v want=%q", identity, err, expected)
	}
}

func TestRAGFairQueueAdminSourceWriterMismatchReturnsNoIdentity(t *testing.T) {
	state := &fairQueueFenceDriverState{connIDs: []int64{41}}
	db, _ := newFairQueueFenceTestStore(t, state)
	admin := &FairQueueAdminStore{db: db}
	source, err := admin.BindRAGFairQueueSource(strings.Repeat("b", 64))
	if err != nil {
		t.Fatalf("bind shape-valid source: %v", err)
	}
	identity, err := source.ReadWriterIdentity(context.Background())
	if identity != (FairQueueWriterIdentity{}) || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	_, _, closed := state.counts()
	if closed != 1 {
		t.Fatalf("mismatched physical writer closes=%d, want 1", closed)
	}
}

func TestRAGFairQueueAdminSourceDelegatesStartFenceWithoutLosingSession(t *testing.T) {
	state := &fairQueueFenceDriverState{
		connIDs: []int64{7, 7, 7}, getResult: int64(1), releaseResult: int64(1),
	}
	db, writer := newFairQueueFenceTestStore(t, state)
	source, err := (&FairQueueAdminStore{db: db}).BindRAGFairQueueSource(writer)
	if err != nil {
		t.Fatalf("bind admin source: %v", err)
	}
	called := false
	err = source.WithFairQueueOperationStartFence(
		context.Background(), "rag.index", writer,
		func(session *FairQueueOperationStartSession) error {
			called = session != nil && session.conn != nil && session.resource == "rag.index"
			return nil
		},
	)
	if err != nil || !called {
		t.Fatalf("start fence called=%v err=%v", called, err)
	}
	get, release, closed := state.counts()
	if get != 1 || release != 1 || closed != 0 {
		t.Fatalf("GET=%d RELEASE=%d physical-close=%d", get, release, closed)
	}
}

func TestCountValidRunningRAGIndexTasksUsesCanonicalFullFence(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	seedRAGTaskDocument(t, st, "doc_admin_running", 3)
	claim, err := st.ClaimRAGIndexTask(context.Background(), "admin-running", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	count, err := st.countValidRunningRAGIndexTasksOn(context.Background(), st.db)
	if err != nil || count != 1 {
		t.Fatalf("valid running count=%d err=%v", count, err)
	}
	if _, err := st.db.ExecContext(context.Background(),
		`UPDATE rag_index_tasks SET heartbeat_at=NULL WHERE id=?`, claim.Task.ID); err != nil {
		t.Fatal(err)
	}
	count, err = st.countValidRunningRAGIndexTasksOn(context.Background(), st.db)
	if err != nil || count != 0 {
		t.Fatalf("pseudo-running count=%d err=%v", count, err)
	}
}

func TestRAGFairQueueContractRequiresAttestationBeforeStoreAccess(t *testing.T) {
	admin := &FairQueueAdminStore{}
	_, err := admin.ApplyRAGFairQueueContract(context.Background(), RAGFairQueueContractAttestation{})
	if !errors.Is(err, ErrRAGFairQueueContractAttestationRequired) {
		t.Fatalf("missing attestation error=%v", err)
	}
}

func TestRAGFairQueueContractAggregateReportHasNoRowIdentity(t *testing.T) {
	ctx := context.Background()
	store, err := NewDBStore("sqlite", "file:"+t.TempDir()+"/contract.db?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	statements := []string{
		`CREATE TABLE users (id TEXT PRIMARY KEY)`,
		`CREATE TABLE rag_kbs (id TEXT PRIMARY KEY,user_id TEXT NOT NULL)`,
		`CREATE TABLE rag_documents (id TEXT PRIMARY KEY,kb_id TEXT NOT NULL)`,
		`CREATE TABLE rag_index_tasks (
			id INTEGER PRIMARY KEY,doc_id TEXT NOT NULL,user_id TEXT,
			status TEXT NOT NULL,claim_generation BIGINT NOT NULL,
			dispatch_generation BIGINT NOT NULL,dispatched_at TIMESTAMP)`,
		`INSERT INTO users (id) VALUES ('u-good'),('u-claimed')`,
		`INSERT INTO rag_kbs (id,user_id) VALUES
			('kb-good','u-good'),('kb-blank','   '),('kb-missing-user','u-missing-user')`,
		`INSERT INTO rag_documents (id,kb_id) VALUES
			('doc-missing','kb-good'),('doc-wrong','kb-good'),('doc-good','kb-good'),
			('doc-blank-task','kb-good'),('doc-blank-owner','kb-blank'),
			('doc-missing-owner-user','kb-missing-user')`,
		// Missing tenant, non-positive/old generation, and a legacy marker.
		`INSERT INTO rag_index_tasks VALUES
			(1,'doc-missing',NULL,'PENDING',0,0,CURRENT_TIMESTAMP)`,
		// Wrong tenant, wrong RUNNING generation, and a legacy marker.
		`INSERT INTO rag_index_tasks VALUES
			(2,'doc-wrong','u-wrong','RUNNING',3,2,CURRENT_TIMESTAMP)`,
		// Canonical task whose document route cannot resolve an owner.
		`INSERT INTO rag_index_tasks VALUES
			(3,'doc-orphan','u-orphan','FAILED',1,1,NULL)`,
		`INSERT INTO rag_index_tasks VALUES
			(4,'doc-good','u-good','PENDING',0,1,NULL)`,
		// This row otherwise satisfies the PENDING invariant, but no later retry
		// or reset can allocate a generation after MaxInt64.
		`INSERT INTO rag_index_tasks VALUES
			(5,'doc-good','u-good','PENDING',9223372036854775806,9223372036854775807,NULL)`,
		// Whitespace-only tenant/owner values are unresolved, not canonical IDs.
		`INSERT INTO rag_index_tasks VALUES
			(6,'doc-blank-task','   ','FAILED',1,1,NULL)`,
		`INSERT INTO rag_index_tasks VALUES
			(7,'doc-blank-owner','u-claimed','FAILED',1,1,NULL)`,
		// A negative claim generation and a byte-distinct owner must fail closed.
		`INSERT INTO rag_index_tasks VALUES
			(8,'doc-good','u-good','PENDING',-1,1,NULL)`,
		`INSERT INTO rag_index_tasks VALUES
			(9,'doc-good','U-GOOD','FAILED',1,1,NULL)`,
		// A non-empty KB owner that is absent from users cannot be part of the
		// authoritative owner universe used by bounded tenant pagination.
		`INSERT INTO rag_index_tasks VALUES
			(10,'doc-missing-owner-user','u-missing-user','FAILED',1,1,NULL)`,
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("statement %q: %v", statement, err)
		}
	}
	report, err := queryRAGFairQueueContractAggregates(ctx, store.db)
	if err != nil {
		t.Fatal(err)
	}
	if report.TaskCount != 10 || report.MissingUserIDCount != 2 ||
		report.UnresolvedOwnerCount != 3 || report.OwnerMismatchCount != 2 ||
		report.NonPositiveGenerationCount != 2 ||
		report.ExhaustedGenerationCount != 1 ||
		report.PendingGenerationMismatchCount != 1 ||
		report.RunningGenerationMismatchCount != 1 ||
		report.PendingDispatchMarkerCount != 1 || report.RunningDispatchMarkerCount != 1 ||
		report.RemainingCount != 9 {
		t.Fatalf("unexpected aggregate report: %+v", report)
	}
	if pages := (report.TaskCount + ragFairQueueContractPageSize - 1) /
		ragFairQueueContractPageSize; pages != 1 {
		t.Fatalf("aggregate page count=%d", pages)
	}
}

func TestRAGFairQueueExpandMetadataRequiresExactShapes(t *testing.T) {
	exact := map[string]ragFairQueueColumnDefinition{
		"user_id": {
			nullable: "YES", dataType: "varchar", columnType: "varchar(120)",
			characterMaximumLength: sql.NullInt64{Int64: 120, Valid: true},
		},
		"dispatched_at": {
			nullable: "YES", dataType: "datetime", columnType: "datetime(6)",
			datetimePrecision: sql.NullInt64{Int64: 6, Valid: true},
		},
		"dispatch_generation": {
			nullable: "NO", dataType: "bigint", columnType: "bigint",
			defaultValue: sql.NullString{String: "1", Valid: true},
		},
	}
	ready, nullable := ragFairQueueExpandDefinitionsReady(exact)
	if !ready || !nullable {
		t.Fatalf("exact metadata ready=%v nullable=%v", ready, nullable)
	}

	wrongLength := cloneRAGFairQueueColumnDefinitions(exact)
	item := wrongLength["user_id"]
	item.columnType = "varchar(1)"
	item.characterMaximumLength.Int64 = 1
	wrongLength["user_id"] = item
	if ready, _ := ragFairQueueExpandDefinitionsReady(wrongLength); ready {
		t.Fatal("VARCHAR(1) user_id passed the contract")
	}

	wrongPrecision := cloneRAGFairQueueColumnDefinitions(exact)
	item = wrongPrecision["dispatched_at"]
	item.columnType = "datetime"
	item.datetimePrecision.Int64 = 0
	wrongPrecision["dispatched_at"] = item
	if ready, _ := ragFairQueueExpandDefinitionsReady(wrongPrecision); ready {
		t.Fatal("DATETIME(0) dispatched_at passed the contract")
	}

	unsignedGeneration := cloneRAGFairQueueColumnDefinitions(exact)
	item = unsignedGeneration["dispatch_generation"]
	item.columnType = "bigint unsigned"
	unsignedGeneration["dispatch_generation"] = item
	if ready, _ := ragFairQueueExpandDefinitionsReady(unsignedGeneration); ready {
		t.Fatal("incompatible unsigned generation passed the contract")
	}
}

func cloneRAGFairQueueColumnDefinitions(
	source map[string]ragFairQueueColumnDefinition,
) map[string]ragFairQueueColumnDefinition {
	cloned := make(map[string]ragFairQueueColumnDefinition, len(source))
	for name, definition := range source {
		cloned[name] = definition
	}
	return cloned
}

func TestFairQueueOperationRecordRejectsForceDeadlineMutation(t *testing.T) {
	first := time.Date(2026, 8, 2, 0, 0, 0, 123456000, time.UTC)
	second := first.Add(time.Second)
	expected := FairQueueOperationRecord{
		Resource: "rag.index", OperationID: "11111111111111111111111111111111",
		Kind: FairQueueOperationForceRebuild, Phase: FairQueueOperationActive,
		CurrentWriterFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ForceNotBefore:           &first, Version: 1, CreatedAt: first, UpdatedAt: first,
	}
	current := expected
	current.Version++
	current.ForceNotBefore = &second
	current.ForceDeletePassComplete = true
	if fairQueueOperationMonotonicFrom(expected, current) {
		t.Fatal("changed force not-before was accepted as idempotent progress")
	}
}
