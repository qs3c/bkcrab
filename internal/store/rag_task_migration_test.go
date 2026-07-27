package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type ragTaskMigrationColumnInfo struct {
	columnType string
	notNull    bool
}

func ragTaskMigrationColumn(t *testing.T, st *DBStore, table, column string) (ragTaskMigrationColumnInfo, bool) {
	t.Helper()
	rows, err := st.db.QueryContext(context.Background(), fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		t.Fatalf("inspect %s.%s: %v", table, column, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan %s schema: %v", table, err)
		}
		if name == column {
			return ragTaskMigrationColumnInfo{
				columnType: strings.ToUpper(columnType),
				notNull:    notNull != 0,
			}, true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s schema: %v", table, err)
	}
	return ragTaskMigrationColumnInfo{}, false
}

func ragTaskMigrationIndex(t *testing.T, st *DBStore, table, indexName string) (bool, bool, []string) {
	t.Helper()
	rows, err := st.db.QueryContext(context.Background(), fmt.Sprintf(`PRAGMA index_list(%s)`, table))
	if err != nil {
		t.Fatalf("list %s indexes: %v", table, err)
	}
	defer rows.Close()
	found := false
	unique := false
	for rows.Next() {
		var seq, isUnique, partial int
		var name, origin string
		if err := rows.Scan(&seq, &name, &isUnique, &origin, &partial); err != nil {
			t.Fatalf("scan %s indexes: %v", table, err)
		}
		if name == indexName {
			found = true
			unique = isUnique != 0
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s indexes: %v", table, err)
	}
	if !found {
		return false, false, nil
	}

	columnRows, err := st.db.QueryContext(context.Background(), fmt.Sprintf(`PRAGMA index_info(%s)`, indexName))
	if err != nil {
		t.Fatalf("inspect index %s: %v", indexName, err)
	}
	defer columnRows.Close()
	var columns []string
	for columnRows.Next() {
		var seq, cid int
		var name string
		if err := columnRows.Scan(&seq, &cid, &name); err != nil {
			t.Fatalf("scan index %s: %v", indexName, err)
		}
		columns = append(columns, name)
	}
	if err := columnRows.Err(); err != nil {
		t.Fatalf("read index %s: %v", indexName, err)
	}
	return true, unique, columns
}

func seedLegacyRAGTaskMigrationFixture(t *testing.T, st *DBStore) map[string]int64 {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC)
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_kbs
		(id,user_id,name,description,embed_provider,embed_model,embed_dims,chunk_size,chunk_overlap,status,created_at,updated_at)
		VALUES ('kb_task_migration','u_task_migration','legacy tasks','','system','embed-v1',768,512,64,'active',?,?)`, base, base); err != nil {
		t.Fatalf("seed legacy KB: %v", err)
	}
	documents := []struct {
		id      string
		status  string
		version int64
		indexed *time.Time
	}{
		{id: "doc_task_history", status: "DONE", version: 7, indexed: &base},
		{id: "doc_task_multiple", status: "PENDING", version: 4},
		{id: "doc_task_snapshot_failure", status: "PENDING", version: 11},
	}
	for _, document := range documents {
		if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_documents
			(id,kb_id,file_name,file_type,file_size,object_key,status,error_msg,chunk_count,token_count,version,uploaded_at,indexed_at)
			VALUES (?,?,?,'md',12,?,?, '',0,0,?,?,?)`,
			document.id, "kb_task_migration", document.id+".md", "rag/u/kb/"+document.id+".md",
			document.status, document.version, base, document.indexed); err != nil {
			t.Fatalf("seed legacy document %s: %v", document.id, err)
		}
	}

	insertTask := func(docID, status string, retryCount, maxRetry int, createdAt time.Time) int64 {
		t.Helper()
		result, err := st.db.ExecContext(ctx, `INSERT INTO rag_index_tasks
			(doc_id,status,retry_count,max_retry,error_msg,created_at,started_at,finished_at)
			VALUES (?,?,?,?, '',?,?,?)`, docID, status, retryCount, maxRetry, createdAt,
			func() any {
				if status == "RUNNING" {
					return createdAt
				}
				return nil
			}(), func() any {
				if status == "DONE" || status == "FAILED" {
					return createdAt.Add(time.Minute)
				}
				return nil
			}())
		if err != nil {
			t.Fatalf("seed legacy task %s/%s: %v", docID, status, err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatalf("legacy task id: %v", err)
		}
		return id
	}

	// Multiple historical terminal rows plus one RUNNING row. Only the RUNNING
	// survivor may be made runnable in the new schema.
	insertTask("doc_task_history", "DONE", 0, 3, base.Add(time.Minute))
	insertTask("doc_task_history", "DONE", 1, 3, base.Add(2*time.Minute))
	historySurvivor := insertTask("doc_task_history", "RUNNING", 2, 5, base.Add(3*time.Minute))

	// The final two rows deliberately tie on created_at. The greater id is the
	// latest row under the required (created_at,id) ordering.
	insertTask("doc_task_multiple", "PENDING", 0, 4, base.Add(4*time.Minute))
	insertTask("doc_task_multiple", "RUNNING", 1, 4, base.Add(5*time.Minute))
	multipleSurvivor := insertTask("doc_task_multiple", "PENDING", 1, 4, base.Add(5*time.Minute))

	insertTask("missing_document", "PENDING", 0, 3, base.Add(6*time.Minute))
	failureSurvivor := insertTask("doc_task_snapshot_failure", "RUNNING", 1, 3, base.Add(7*time.Minute))

	return map[string]int64{
		"doc_task_history":          historySurvivor,
		"doc_task_multiple":         multipleSurvivor,
		"doc_task_snapshot_failure": failureSurvivor,
	}
}

func ragTaskMigrationSnapshot(docID string, docVersion int64) *RAGDocumentVersionRecord {
	return &RAGDocumentVersionRecord{
		DocID:                        docID,
		DocVersion:                   docVersion,
		Status:                       RAGDocumentVersionPending,
		SourceSHA256:                 strings.Repeat("a", 64),
		ParseMode:                    RAGParseModeStandard,
		ChunkSize:                    512,
		ChunkOverlap:                 64,
		ParserVersion:                "parser-migration-v1",
		SplitterVersion:              "splitter-migration-v1",
		ParseFingerprint:             strings.Repeat("b", 64),
		IndexFingerprint:             strings.Repeat("c", 64),
		VisionModel:                  "vision-v1",
		VisionProviderFingerprint:    strings.Repeat("d", 64),
		VisionPromptVersion:          "vision-prompt-v1",
		TextModel:                    "text-v1",
		TextProviderFingerprint:      strings.Repeat("e", 64),
		EnrichmentPromptVersion:      "enrichment-prompt-v1",
		EnrichmentEnabled:            true,
		MaxDocumentAIRequests:        300,
		MaxDocumentAITokens:          200_000,
		MaxDocumentAICostMicroUSD:    1_000_000,
		EmbeddingProvider:            "system",
		EmbeddingModel:               "embed-v1",
		EmbeddingDimensions:          768,
		EmbeddingContractFingerprint: strings.Repeat("f", 64),
	}
}

func ragLegacyTaskMigrationState(t *testing.T, st *DBStore) string {
	t.Helper()
	ctx := context.Background()
	var state strings.Builder
	rows, err := st.db.QueryContext(ctx, `SELECT id,doc_id,
		COALESCE(CAST(doc_version AS TEXT),'NULL'),status,retry_count,max_retry,error_msg
		FROM rag_index_tasks ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var docID, docVersion, status, errorMsg string
		var retryCount, maxRetry int
		if err := rows.Scan(&id, &docID, &docVersion, &status, &retryCount, &maxRetry, &errorMsg); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		fmt.Fprintf(&state, "task:%d:%s:%s:%s:%d:%d:%s\n",
			id, docID, docVersion, status, retryCount, maxRetry, errorMsg)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err = st.db.QueryContext(ctx, `SELECT id,version,status,source_sha256,
		active_version,index_format_version,processing_stage,error_msg
		FROM rag_documents ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, status, sourceSHA256, stage, errorMsg string
		var version, activeVersion int64
		var indexFormatVersion int
		if err := rows.Scan(&id, &version, &status, &sourceSHA256, &activeVersion,
			&indexFormatVersion, &stage, &errorMsg); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&state, "doc:%s:%d:%s:%s:%d:%d:%s:%s\n", id, version,
			status, sourceSHA256, activeVersion, indexFormatVersion, stage, errorMsg)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return state.String()
}

func TestRAGTaskMigrationRequiresExplicitOfflineAcknowledgement(t *testing.T) {
	st := openUnmigratedRAGSQLite(t)
	installLegacyRAGSchema(t, st)
	seedLegacyRAGTaskMigrationFixture(t, st)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	before := ragLegacyTaskMigrationState(t, st)
	builderCalled := false
	builder := RAGLegacyTaskSnapshotBuilder(func(context.Context, *RAGDocumentRecord, int64) (*RAGDocumentVersionRecord, error) {
		builderCalled = true
		return nil, errors.New("must not be called without offline acknowledgement")
	})
	if err := st.MigrateLegacyRAGIndexTasks(ctx, builder, false); !errors.Is(err, ErrRAGLegacyTaskMigrationRequired) {
		t.Fatalf("migration without acknowledgement err=%v", err)
	}
	if builderCalled {
		t.Fatal("snapshot builder ran before offline migration was acknowledged")
	}
	if after := ragLegacyTaskMigrationState(t, st); after != before {
		t.Fatalf("unacknowledged migration changed state\nbefore:\n%s\nafter:\n%s", before, after)
	}

	if err := st.MigrateLegacyRAGIndexTasks(ctx, nil, true); !errors.Is(err, ErrRAGLegacySnapshotBuilder) {
		t.Fatalf("acknowledged migration without builder err=%v", err)
	}
	if after := ragLegacyTaskMigrationState(t, st); after != before {
		t.Fatalf("nil-builder preflight changed state\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestRAGTaskMigrationCanonicalSchemaNeedsNoAcknowledgement(t *testing.T) {
	st := openTestDB(t)
	if err := st.MigrateLegacyRAGIndexTasks(context.Background(), nil, false); err != nil {
		t.Fatalf("canonical schema migration check: %v", err)
	}
}

func TestRAGTaskMigrationEmptyExpandedSchemaNeedsNoAcknowledgement(t *testing.T) {
	st := openUnmigratedRAGSQLite(t)
	installLegacyRAGSchema(t, st)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	before, ok := ragTaskMigrationColumn(t, st, "rag_index_tasks", "doc_version")
	if !ok || before.notNull {
		t.Fatalf("expanded doc_version before contract = %+v, exists=%v", before, ok)
	}
	if err := st.MigrateLegacyRAGIndexTasks(ctx, nil, false); err != nil {
		t.Fatalf("contract empty expanded schema: %v", err)
	}
	after, ok := ragTaskMigrationColumn(t, st, "rag_index_tasks", "doc_version")
	if !ok || !after.notNull {
		t.Fatalf("doc_version after empty contract = %+v, exists=%v", after, ok)
	}
}

func TestRAGTaskMigrationRejectsNonNullRunnableWithoutSnapshot(t *testing.T) {
	st := openUnmigratedRAGSQLite(t)
	installLegacyRAGSchema(t, st)
	survivors := seedLegacyRAGTaskMigrationFixture(t, st)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	missingSnapshotTask := survivors["doc_task_history"]
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_index_tasks SET doc_version=99
		WHERE id=? AND status='RUNNING'`, missingSnapshotTask); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_documents SET version=99
		WHERE id='doc_task_history'`); err != nil {
		t.Fatal(err)
	}
	builder := RAGLegacyTaskSnapshotBuilder(func(_ context.Context, doc *RAGDocumentRecord, version int64) (*RAGDocumentVersionRecord, error) {
		return ragTaskMigrationSnapshot(doc.ID, version), nil
	})
	if err := st.MigrateLegacyRAGIndexTasks(ctx, builder, true); err == nil || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("missing non-null snapshot contract err=%v", err)
	}
	column, ok := ragTaskMigrationColumn(t, st, "rag_index_tasks", "doc_version")
	if !ok || column.notNull {
		t.Fatalf("contract tightened despite missing snapshot: %+v, exists=%v", column, ok)
	}
	task, err := st.GetRAGIndexTask(ctx, missingSnapshotTask)
	if err != nil || task.Status != "RUNNING" || task.DocVersion != 99 {
		t.Fatalf("pre-existing malformed task was silently blessed: %+v, %v", task, err)
	}
}

func TestRAGTaskMigrationInvalidBuilderSnapshotCannotRemainRunnable(t *testing.T) {
	st := openUnmigratedRAGSQLite(t)
	installLegacyRAGSchema(t, st)
	survivors := seedLegacyRAGTaskMigrationFixture(t, st)
	ctx := context.Background()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	builder := RAGLegacyTaskSnapshotBuilder(func(_ context.Context, doc *RAGDocumentRecord, version int64) (*RAGDocumentVersionRecord, error) {
		snapshot := ragTaskMigrationSnapshot(doc.ID, version)
		snapshot.EmbeddingContractFingerprint = "present-but-invalid"
		return snapshot, nil
	})
	if err := st.MigrateLegacyRAGIndexTasks(ctx, builder, true); err != nil {
		t.Fatalf("invalid per-document snapshot should be terminalized, got %v", err)
	}
	for docID, taskID := range survivors {
		task, err := st.GetRAGIndexTask(ctx, taskID)
		if err != nil {
			t.Fatalf("task %s: %v", docID, err)
		}
		if task.Status != "FAILED" || task.ErrorMsg == "" {
			t.Fatalf("invalid snapshot task %s remained runnable: %+v", docID, task)
		}
		var runnable int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_index_tasks
			WHERE doc_id=? AND status IN ('PENDING','RUNNING')`, docID).Scan(&runnable); err != nil {
			t.Fatal(err)
		}
		if runnable != 0 {
			t.Fatalf("invalid snapshot left %d runnable task(s) for %s", runnable, docID)
		}
	}
}

func TestRAGTaskMigrationExpandBackfillAndContractIsIdempotent(t *testing.T) {
	st := openUnmigratedRAGSQLite(t)
	installLegacyRAGSchema(t, st)
	survivorIDs := seedLegacyRAGTaskMigrationFixture(t, st)
	ctx := context.Background()

	// Migrate performs only the safe expand step because the runtime snapshot
	// builder is not available during base database construction.
	for pass := 1; pass <= 2; pass++ {
		if err := st.Migrate(ctx); err != nil {
			t.Fatalf("expand migration pass %d: %v", pass, err)
		}
	}
	docVersionColumn, ok := ragTaskMigrationColumn(t, st, "rag_index_tasks", "doc_version")
	if !ok || docVersionColumn.columnType != "BIGINT" || docVersionColumn.notNull {
		t.Fatalf("expanded doc_version = %+v, exists=%v; want nullable BIGINT", docVersionColumn, ok)
	}
	for _, column := range []string{
		"retry_count", "max_retry", "claim_generation", "lease_owner",
		"lease_until", "heartbeat_at", "next_run_at",
	} {
		if _, exists := ragTaskMigrationColumn(t, st, "rag_index_tasks", column); !exists {
			t.Errorf("expanded rag_index_tasks missing %s", column)
		}
	}
	if _, exists := ragTaskMigrationColumn(t, st, "rag_index_tasks", "attempt_count"); exists {
		t.Error("rag_index_tasks must not add the GC-only attempt_count column")
	}
	if _, exists := ragTaskMigrationColumn(t, st, "rag_index_gc_tasks", "attempt_count"); !exists {
		t.Error("rag_index_gc_tasks lost its independent attempt_count column")
	}
	if found, unique, columns := ragTaskMigrationIndex(t, st, "rag_index_tasks", "uq_rag_index_tasks_doc_version"); found {
		t.Fatalf("unique task/version constraint was installed before backfill: unique=%v columns=%v", unique, columns)
	}
	if found, unique, columns := ragTaskMigrationIndex(t, st, "rag_index_tasks", "idx_rag_index_tasks_runnable"); !found || unique || strings.Join(columns, ",") != "status,next_run_at,lease_until,created_at" {
		t.Fatalf("expanded runnable index = found=%v unique=%v columns=%v", found, unique, columns)
	}

	// Model partially migrated installations too: a known old physical version
	// must be terminalized instead of being left RUNNING/PENDING forever when a
	// fresh survivor version is allocated.
	for _, fixture := range []struct {
		docID   string
		version int64
		status  string
	}{
		{docID: "doc_task_multiple", version: 4, status: RAGDocumentVersionRunning},
		{docID: "doc_task_snapshot_failure", version: 11, status: RAGDocumentVersionPending},
	} {
		version := ragTaskMigrationSnapshot(fixture.docID, fixture.version)
		if err := st.CreateRAGDocumentVersion(ctx, version); err != nil {
			t.Fatalf("seed old version %s/%d: %v", fixture.docID, fixture.version, err)
		}
		if _, err := st.db.ExecContext(ctx, `UPDATE rag_document_versions SET status=?
			WHERE doc_id=? AND doc_version=?`, fixture.status, fixture.docID, fixture.version); err != nil {
			t.Fatalf("set old version state %s/%d: %v", fixture.docID, fixture.version, err)
		}
	}

	buildCalls := make(map[string][]int64)
	builder := RAGLegacyTaskSnapshotBuilder(func(_ context.Context, doc *RAGDocumentRecord, nextVersion int64) (*RAGDocumentVersionRecord, error) {
		buildCalls[doc.ID] = append(buildCalls[doc.ID], nextVersion)
		if doc.ID == "doc_task_snapshot_failure" {
			return nil, errors.New("fixture cannot read source object")
		}
		return ragTaskMigrationSnapshot(doc.ID, nextVersion), nil
	})
	if err := st.MigrateLegacyRAGIndexTasks(ctx, builder, true); err != nil {
		t.Fatalf("runtime task migration pass 1: %v", err)
	}
	firstCallCount := 0
	for _, calls := range buildCalls {
		firstCallCount += len(calls)
	}
	if err := st.MigrateLegacyRAGIndexTasks(ctx, builder, true); err != nil {
		t.Fatalf("runtime task migration pass 2: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("expand after contract: %v", err)
	}
	secondCallCount := 0
	for _, calls := range buildCalls {
		secondCallCount += len(calls)
	}
	if secondCallCount != firstCallCount {
		t.Fatalf("idempotent runtime migration rebuilt snapshots: calls %d -> %d", firstCallCount, secondCallCount)
	}

	for docID, wantNextVersion := range map[string]int64{
		"doc_task_history":          8,
		"doc_task_multiple":         5,
		"doc_task_snapshot_failure": 12,
	} {
		calls := buildCalls[docID]
		if len(calls) != 1 || calls[0] != wantNextVersion {
			t.Errorf("builder calls for %s = %v, want [%d]", docID, calls, wantNextVersion)
		}
	}
	if calls := buildCalls["missing_document"]; len(calls) != 0 {
		t.Fatalf("snapshot builder called for orphan task: %v", calls)
	}

	docVersionColumn, ok = ragTaskMigrationColumn(t, st, "rag_index_tasks", "doc_version")
	if !ok || docVersionColumn.columnType != "BIGINT" || !docVersionColumn.notNull {
		t.Fatalf("contracted doc_version = %+v, exists=%v; want NOT NULL BIGINT", docVersionColumn, ok)
	}
	if found, unique, columns := ragTaskMigrationIndex(t, st, "rag_index_tasks", "uq_rag_index_tasks_doc_version"); !found || !unique || strings.Join(columns, ",") != "doc_id,doc_version" {
		t.Fatalf("task/version unique index = found=%v unique=%v columns=%v", found, unique, columns)
	}
	var nullVersions, duplicateVersions int
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_index_tasks WHERE doc_version IS NULL`).Scan(&nullVersions); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM (
		SELECT doc_id,doc_version FROM rag_index_tasks GROUP BY doc_id,doc_version HAVING COUNT(*)>1
	) duplicates`).Scan(&duplicateVersions); err != nil {
		t.Fatal(err)
	}
	if nullVersions != 0 || duplicateVersions != 0 {
		t.Fatalf("contract validation: null versions=%d duplicate versions=%d", nullVersions, duplicateVersions)
	}

	for docID, want := range map[string]struct {
		version    int64
		retryCount int
		maxRetry   int
	}{
		"doc_task_history":  {version: 8, retryCount: 2, maxRetry: 5},
		"doc_task_multiple": {version: 5, retryCount: 1, maxRetry: 4},
	} {
		var runnableCount int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_index_tasks
			WHERE doc_id=? AND status IN ('PENDING','RUNNING')`, docID).Scan(&runnableCount); err != nil {
			t.Fatal(err)
		}
		if runnableCount != 1 {
			t.Errorf("runnable tasks for %s = %d, want 1", docID, runnableCount)
		}
		task, err := st.GetRAGIndexTask(ctx, survivorIDs[docID])
		if err != nil {
			t.Errorf("survivor %s: %v", docID, err)
			continue
		}
		if task.DocVersion != want.version || task.Status != "PENDING" ||
			task.RetryCount != want.retryCount || task.MaxRetry != want.maxRetry ||
			task.ClaimGeneration != 0 || task.LeaseOwner != "" || task.LeaseUntil != nil {
			t.Errorf("survivor %s = %+v", docID, task)
		}
		version, err := st.GetRAGDocumentVersion(ctx, docID, want.version)
		if err != nil {
			t.Errorf("survivor snapshot %s/%d: %v", docID, want.version, err)
			continue
		}
		if version.Status != RAGDocumentVersionPending || version.ParserVersion != "parser-migration-v1" ||
			version.EmbeddingProvider != "system" || version.EmbeddingModel != "embed-v1" ||
			version.EmbeddingDimensions != 768 || version.SourceSHA256 == "" {
			t.Errorf("incomplete survivor snapshot %s/%d: %+v", docID, want.version, version)
		}
	}

	for _, docID := range []string{"missing_document", "doc_task_snapshot_failure"} {
		var runnableCount int
		if err := st.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rag_index_tasks
			WHERE doc_id=? AND status IN ('PENDING','RUNNING')`, docID).Scan(&runnableCount); err != nil {
			t.Fatal(err)
		}
		if runnableCount != 0 {
			t.Errorf("invalid legacy task %s remained runnable", docID)
		}
	}
	failureDoc, err := st.GetRAGDocument(ctx, "doc_task_snapshot_failure")
	if err != nil {
		t.Fatal(err)
	}
	if failureDoc.Status != "FAILED" || failureDoc.ActiveVersion != 0 || failureDoc.ErrorMsg == "" {
		t.Fatalf("snapshot failure document = %+v; want FAILED needs-reindex state", failureDoc)
	}
	failureTask, err := st.GetRAGIndexTask(ctx, survivorIDs["doc_task_snapshot_failure"])
	if err != nil || failureTask.Status != "FAILED" || failureTask.ErrorMsg == "" {
		t.Fatalf("snapshot failure task = %+v, %v; want retained FAILED audit row", failureTask, err)
	}

	oldMultiple, err := st.GetRAGDocumentVersion(ctx, "doc_task_multiple", 4)
	if err != nil || oldMultiple.Status != RAGDocumentVersionSuperseded {
		t.Fatalf("old RUNNING version = %+v, %v; want SUPERSEDED", oldMultiple, err)
	}
	oldFailure, err := st.GetRAGDocumentVersion(ctx, "doc_task_snapshot_failure", 11)
	if err != nil || (oldFailure.Status != RAGDocumentVersionFailed && oldFailure.Status != RAGDocumentVersionSuperseded) {
		t.Fatalf("snapshot-failure PENDING version = %+v, %v; want terminal", oldFailure, err)
	}
	legacyActive, err := st.GetRAGDocument(ctx, "doc_task_history")
	if err != nil || legacyActive.ActiveVersion != 7 || legacyActive.Version != 8 ||
		legacyActive.SourceSHA256 != strings.Repeat("a", 64) {
		t.Fatalf("legacy active version changed incorrectly: %+v, %v", legacyActive, err)
	}
	legacyVersion, err := st.GetRAGDocumentVersion(ctx, "doc_task_history", 7)
	if err != nil || legacyVersion.Status != RAGDocumentVersionDone {
		t.Fatalf("pinned legacy version = %+v, %v", legacyVersion, err)
	}

	// The final constraint must reject duplicate physical fencing epochs.
	if _, err := st.db.ExecContext(ctx, `INSERT INTO rag_index_tasks
		(doc_id,doc_version,status,retry_count,max_retry,claim_generation,lease_owner,error_msg,created_at)
		VALUES ('doc_task_history',8,'FAILED',0,3,0,'','duplicate',CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("duplicate (doc_id,doc_version) task was accepted")
	}
}

func TestRAGTaskCanonicalSchemaAcrossDialects(t *testing.T) {
	cases := []struct {
		name       string
		statements []string
	}{
		{name: "sqlite", statements: (&DBStore{dialect: "sqlite"}).migrationSQL()},
		{name: "postgres", statements: (&DBStore{dialect: "postgres"}).migrationSQL()},
		{name: "mysql", statements: mysqlMigrationSQL()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allDDL := strings.ToLower(strings.Join(tc.statements, "\n"))
			taskDDL := ""
			for _, statement := range tc.statements {
				lower := strings.ToLower(statement)
				if strings.Contains(lower, "create table if not exists rag_index_tasks (") {
					taskDDL = lower
					break
				}
			}
			if taskDDL == "" {
				t.Fatal("canonical DDL has no rag_index_tasks table")
			}
			for _, token := range []string{
				"doc_version bigint not null",
				"retry_count integer not null default 0",
				"max_retry integer not null default 3",
				"claim_generation bigint not null default 0",
				"lease_owner",
				"lease_until",
				"heartbeat_at",
				"next_run_at",
			} {
				if !strings.Contains(taskDDL, token) {
					t.Errorf("rag_index_tasks DDL missing %q", token)
				}
			}
			if strings.Contains(taskDDL, "attempt_count") {
				t.Error("rag_index_tasks DDL contains overlapping attempt_count")
			}
			for _, token := range []string{
				"idx_rag_index_tasks_runnable",
				"status, next_run_at, lease_until, created_at",
				"idx_rag_index_gc_tasks_runnable",
			} {
				if !strings.Contains(allDDL, token) {
					t.Errorf("canonical DDL missing %q", token)
				}
			}
		})
	}
}
