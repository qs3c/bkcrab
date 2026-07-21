package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func openRAGTaskClaimStore(t *testing.T) *DBStore {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "rag-task-claim.db") +
		"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	st, err := NewDBStore("sqlite", dsn)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func testRAGVersion(docID string, docVersion int64) *RAGDocumentVersionRecord {
	return &RAGDocumentVersionRecord{
		DocID:                        docID,
		DocVersion:                   docVersion,
		Status:                       RAGDocumentVersionPending,
		SourceSHA256:                 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ParseMode:                    RAGParseModeStandard,
		ChunkSize:                    512,
		ChunkOverlap:                 64,
		ParserVersion:                "parser-v1",
		SplitterVersion:              "splitter-v1",
		ParseFingerprint:             "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		IndexFingerprint:             "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		VisionModel:                  "vision-v1",
		VisionProviderFingerprint:    "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		VisionPromptVersion:          "vision-prompt-v1",
		TextModel:                    "text-v1",
		TextProviderFingerprint:      "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		EnrichmentPromptVersion:      "enrich-prompt-v1",
		EnrichmentEnabled:            true,
		MaxDocumentAIRequests:        20,
		MaxDocumentAITokens:          10_000,
		MaxDocumentAICostMicroUSD:    50_000,
		EmbeddingProvider:            "system",
		EmbeddingModel:               "embed-v1",
		EmbeddingDimensions:          3,
		EmbeddingContractFingerprint: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}
}

func seedRAGTaskDocument(t *testing.T, st *DBStore, docID string, maxRetry int) (*RAGDocumentRecord, int64) {
	t.Helper()
	ctx := context.Background()
	ensureRAGLifecycleUser(t, st, "u_claim", "active")
	now := time.Date(2026, 7, 20, 1, 2, 3, 0, time.UTC)
	kb := &RAGKBRecord{
		ID: "kb_" + docID, UserID: "u_claim", Name: "claim", EmbedProvider: "system",
		EmbedModel: "embed-v1", EmbedDims: 3, ChunkSize: 512, ChunkOverlap: 64,
		ParseMode: RAGParseModeStandard, Status: "active", CreatedAt: now,
	}
	if err := st.CreateRAGKB(ctx, kb); err != nil {
		t.Fatalf("create KB: %v", err)
	}
	doc := &RAGDocumentRecord{
		ID: docID, KBID: kb.ID, FileName: "claim.md", FileType: "md", FileSize: 12,
		ObjectKey: "rag/u/kb/" + docID + "/claim.md", Status: "PENDING", Version: 1,
		SourceSHA256: testRAGVersion(docID, 1).SourceSHA256, IndexFormatVersion: 1,
		ProcessingStage: "queued", UploadedAt: now,
	}
	taskID, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, testRAGVersion(docID, 1), maxRetry)
	if err != nil {
		t.Fatalf("seed document task: %v", err)
	}
	return doc, taskID
}

func TestRAGParseArtifactHandleIsFencedImmutableAndSurvivesFailure(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_parse_artifact_handle", 1)
	ctx := context.Background()
	claim, err := st.ClaimRAGIndexTask(ctx, "artifact-handle-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	artifactKey := "rag/u_claim/" + doc.KBID + "/" + doc.ID +
		"/artifacts/" + claim.Version.ParseFingerprint + "/parsed.json"
	if ok, err := st.RecordRAGDocumentParseArtifact(ctx, claim.Fence, artifactKey); err != nil || !ok {
		t.Fatalf("record handle ok=%v err=%v", ok, err)
	}
	if ok, err := st.RecordRAGDocumentParseArtifact(ctx, claim.Fence, artifactKey); err != nil || !ok {
		t.Fatalf("idempotent handle ok=%v err=%v", ok, err)
	}
	if ok, err := st.RecordRAGDocumentParseArtifact(
		ctx, claim.Fence, artifactKey+".different",
	); !errors.Is(err, ErrRAGDocumentVersionConflict) || ok {
		t.Fatalf("conflicting handle ok=%v err=%v", ok, err)
	}
	if ok, err := st.FailRAGIndexTask(ctx, claim.Fence, "after artifact publish"); err != nil || !ok {
		t.Fatalf("fail task ok=%v err=%v", ok, err)
	}
	version, err := st.GetRAGDocumentVersion(ctx, doc.ID, claim.Fence.DocVersion)
	if err != nil || version.Status != RAGDocumentVersionFailed || version.ParseArtifactKey != artifactKey {
		t.Fatalf("failed version=%+v err=%v", version, err)
	}
	if ok, err := st.RecordRAGDocumentParseArtifact(ctx, claim.Fence, artifactKey); err != nil || ok {
		t.Fatalf("stale fence rewrote handle ok=%v err=%v", ok, err)
	}
}

func TestRAGRunnableSnapshotValidationRejectsIncompleteCreate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RAGDocumentVersionRecord)
	}{
		{"invalid parse mode", func(v *RAGDocumentVersionRecord) { v.ParseMode = "advanced" }},
		{"non canonical source hash", func(v *RAGDocumentVersionRecord) { v.SourceSHA256 = "A" + v.SourceSHA256[1:] }},
		{"conflicting source hash", func(v *RAGDocumentVersionRecord) {
			v.SourceSHA256 = "9999999999999999999999999999999999999999999999999999999999999999"
		}},
		{"invalid parse fingerprint", func(v *RAGDocumentVersionRecord) { v.ParseFingerprint = "not-a-sha256" }},
		{"empty embedding model", func(v *RAGDocumentVersionRecord) { v.EmbeddingModel = "" }},
		{"zero embedding dimensions", func(v *RAGDocumentVersionRecord) { v.EmbeddingDimensions = 0 }},
		{"embedding contract differs from KB", func(v *RAGDocumentVersionRecord) { v.EmbeddingModel = "embed-v2" }},
		{"auto without vision contract", func(v *RAGDocumentVersionRecord) {
			v.ParseMode = RAGParseModeAuto
			v.VisionModel = ""
			v.VisionProviderFingerprint = ""
			v.VisionPromptVersion = ""
		}},
		{"enrichment without text contract", func(v *RAGDocumentVersionRecord) {
			v.EnrichmentEnabled = true
			v.TextModel = ""
			v.TextProviderFingerprint = ""
			v.EnrichmentPromptVersion = ""
		}},
		{"zero task budget", func(v *RAGDocumentVersionRecord) { v.MaxDocumentAIRequests = 0 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			st := openRAGTaskClaimStore(t)
			ctx := context.Background()
			docID := "doc_invalid_create"
			ensureRAGLifecycleUser(t, st, "u_claim", "active")
			if err := st.CreateRAGKB(ctx, &RAGKBRecord{
				ID: "kb_invalid_create", UserID: "u_claim", Name: "invalid snapshot",
				EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 3,
				ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeStandard,
				Status: "active", CreatedAt: time.Now().UTC(),
			}); err != nil {
				t.Fatal(err)
			}
			doc := &RAGDocumentRecord{
				ID: docID, KBID: "kb_invalid_create", FileName: "invalid.md", FileType: "md",
				ObjectKey: "rag/u/kb/invalid.md", Status: "PENDING", Version: 1,
				SourceSHA256: testRAGVersion(docID, 1).SourceSHA256, UploadedAt: time.Now().UTC(),
			}
			version := testRAGVersion(docID, 1)
			test.mutate(version)
			if _, err := st.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, version, 3); err == nil {
				t.Fatal("incomplete snapshot was accepted")
			}
			for table, want := range map[string]int{"rag_documents": 0, "rag_document_versions": 0, "rag_index_tasks": 0} {
				var count int
				if err := st.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&count); err != nil {
					t.Fatal(err)
				}
				if count != want {
					t.Fatalf("%s rows=%d after rejected snapshot, want %d", table, count, want)
				}
			}
		})
	}
}

func TestRAGRunnableSnapshotValidationPrecedesAdvanceAndSupersedeWrites(t *testing.T) {
	t.Run("advance", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		doc, taskID := seedRAGTaskDocument(t, st, "doc_invalid_advance", 3)
		invalid := testRAGVersion(doc.ID, 0)
		invalid.IndexFingerprint = "invalid"
		if _, err := st.AdvanceDocumentVersionAndCreateTask(context.Background(), 1, invalid); !errors.Is(err, ErrRAGDocumentVersionIncomplete) {
			t.Fatalf("invalid advance err=%v", err)
		}
		current, err := st.GetRAGDocument(context.Background(), doc.ID)
		if err != nil || current.Version != 1 || current.Status != "PENDING" {
			t.Fatalf("document changed after invalid advance: %+v, %v", current, err)
		}
		task, err := st.GetRAGIndexTask(context.Background(), taskID)
		if err != nil || task.Status != "PENDING" || task.DocVersion != 1 {
			t.Fatalf("task changed after invalid advance: %+v, %v", task, err)
		}
		versions, err := st.ListRAGDocumentVersions(context.Background(), doc.ID)
		if err != nil || len(versions) != 1 || versions[0].Status != RAGDocumentVersionPending {
			t.Fatalf("versions after invalid advance: %+v, %v", versions, err)
		}
	})

	t.Run("provider supersede", func(t *testing.T) {
		st := openRAGTaskClaimStore(t)
		doc, taskID := seedRAGTaskDocument(t, st, "doc_invalid_supersede", 3)
		claim, err := st.ClaimRAGIndexTask(context.Background(), "worker-valid", time.Minute)
		if err != nil || claim == nil {
			t.Fatalf("claim = %+v, %v", claim, err)
		}
		invalid := testRAGVersion(doc.ID, 0)
		invalid.EmbeddingDimensions = 0
		if _, changed, err := st.SupersedeRAGIndexTaskAndCreateVersion(context.Background(), claim.Fence, invalid); !errors.Is(err, ErrRAGDocumentVersionIncomplete) || changed {
			t.Fatalf("invalid supersede changed=%v err=%v", changed, err)
		}
		current, err := st.GetRAGDocument(context.Background(), doc.ID)
		if err != nil || current.Version != 1 || current.Status != "PROCESSING" {
			t.Fatalf("document changed after invalid supersede: %+v, %v", current, err)
		}
		task, err := st.GetRAGIndexTask(context.Background(), taskID)
		if err != nil || task.Status != "RUNNING" || task.DocVersion != 1 {
			t.Fatalf("task changed after invalid supersede: %+v, %v", task, err)
		}
		version, err := st.GetRAGDocumentVersion(context.Background(), doc.ID, 1)
		if err != nil || version.Status != RAGDocumentVersionRunning {
			t.Fatalf("version changed after invalid supersede: %+v, %v", version, err)
		}
	})
}

func TestRAGTaskClaimFailsClosedOnMalformedSnapshot(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, taskID := seedRAGTaskDocument(t, st, "doc_invalid_claim", 3)
	ctx := context.Background()
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_document_versions SET
		parse_fingerprint='present-but-not-a-sha256' WHERE doc_id=? AND doc_version=1`, doc.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.MigrateLegacyRAGIndexTasks(ctx, nil, false); !errors.Is(err, ErrRAGDocumentVersionIncomplete) {
		t.Fatalf("contract validation accepted malformed runnable snapshot: %v", err)
	}
	claim, err := st.ClaimRAGIndexTask(ctx, "worker-malformed", time.Minute)
	if err != nil || claim != nil {
		t.Fatalf("malformed snapshot claim=%+v err=%v", claim, err)
	}
	task, err := st.GetRAGIndexTask(ctx, taskID)
	if err != nil || task.Status != "FAILED" || task.ErrorMsg == "" {
		t.Fatalf("malformed task was not terminalized: %+v, %v", task, err)
	}
	current, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || current.Status != "FAILED" || current.ActiveVersion != 0 {
		t.Fatalf("malformed document state: %+v, %v", current, err)
	}
}

func TestRAGTaskClaimRejectsNonPortableWorkerID(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	seedRAGTaskDocument(t, st, "doc_worker_id", 3)
	for _, workerID := range []string{"", " leading-space", string(make([]byte, 97))} {
		if claim, err := st.ClaimRAGIndexTask(context.Background(), workerID, time.Minute); err == nil || claim != nil {
			t.Fatalf("worker id %q claim=%+v err=%v", workerID, claim, err)
		}
	}
	valid, err := st.ClaimRAGIndexTask(context.Background(), "worker-portable", time.Minute)
	if err != nil || valid == nil {
		t.Fatalf("valid worker id claim=%+v err=%v", valid, err)
	}
}

func TestRAGTaskClaimConcurrentAndLease(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	_, taskID := seedRAGTaskDocument(t, st, "doc_claim_concurrent", 3)
	ctx := context.Background()

	start := make(chan struct{})
	claims := make(chan *RAGIndexClaim, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, workerID := range []string{"worker-a", "worker-b"} {
		workerID := workerID
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claim, err := st.ClaimRAGIndexTask(ctx, workerID, 5*time.Minute)
			claims <- claim
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(claims)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent claim: %v", err)
		}
	}
	var winner *RAGIndexClaim
	claimed := 0
	for claim := range claims {
		if claim != nil {
			winner = claim
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("successful claims = %d, want 1", claimed)
	}
	if winner.Fence.TaskID != taskID || winner.Fence.DocVersion != 1 ||
		winner.Fence.ClaimGeneration != 1 || winner.Fence.LeaseOwner == "" {
		t.Fatalf("winner fence = %+v", winner.Fence)
	}
	if winner.Version.Status != RAGDocumentVersionRunning {
		t.Fatalf("claimed version status = %q", winner.Version.Status)
	}

	blocked, err := st.ClaimRAGIndexTask(ctx, "worker-c", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if blocked != nil {
		t.Fatalf("unexpired lease was reclaimed: %+v", blocked.Fence)
	}
	stale := winner.Fence
	stale.LeaseOwner = "worker-c"
	if ok, err := st.HeartbeatRAGIndexTask(ctx, stale, 5*time.Minute); err != nil || ok {
		t.Fatalf("stale heartbeat = %v, %v; want false, nil", ok, err)
	}
	if ok, err := st.HeartbeatRAGIndexTask(ctx, winner.Fence, 5*time.Minute); err != nil || !ok {
		t.Fatalf("owner heartbeat = %v, %v; want true, nil", ok, err)
	}
}

func TestRAGTaskClaimExpiredLeaseAllocatesFreshVersionAndRejectsStaleFence(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_claim_reclaim", 3)
	ctx := context.Background()

	first, err := st.ClaimRAGIndexTask(ctx, "worker-old", 5*time.Minute)
	if err != nil || first == nil {
		t.Fatalf("first claim = %+v, %v", first, err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_index_tasks SET lease_until = '2000-01-01 00:00:00' WHERE id = ?`, first.Fence.TaskID); err != nil {
		t.Fatalf("expire lease: %v", err)
	}

	reclaimed, err := st.ClaimRAGIndexTask(ctx, "worker-new", 5*time.Minute)
	if err != nil || reclaimed == nil {
		t.Fatalf("reclaim = %+v, %v", reclaimed, err)
	}
	if reclaimed.Fence.TaskID != first.Fence.TaskID || reclaimed.Fence.DocVersion != 2 ||
		reclaimed.Fence.ClaimGeneration != 2 || reclaimed.Task.RetryCount != 1 {
		t.Fatalf("reclaimed claim = %+v task=%+v", reclaimed.Fence, reclaimed.Task)
	}
	oldVersion, err := st.GetRAGDocumentVersion(ctx, doc.ID, 1)
	if err != nil || oldVersion.Status != RAGDocumentVersionSuperseded {
		t.Fatalf("old version = %+v, %v", oldVersion, err)
	}
	newVersion, err := st.GetRAGDocumentVersion(ctx, doc.ID, 2)
	if err != nil || newVersion.Status != RAGDocumentVersionRunning {
		t.Fatalf("new version = %+v, %v", newVersion, err)
	}
	if newVersion.ParseFingerprint != oldVersion.ParseFingerprint ||
		newVersion.EmbeddingContractFingerprint != oldVersion.EmbeddingContractFingerprint {
		t.Fatalf("immutable snapshot was not copied: old=%+v new=%+v", oldVersion, newVersion)
	}
	currentDoc, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || currentDoc.Version != 2 || currentDoc.ActiveVersion != 0 {
		t.Fatalf("document after reclaim = %+v, %v", currentDoc, err)
	}

	activation := RAGIndexActivation{VersionResult: RAGDocumentVersionResult{Status: RAGDocumentVersionDone}}
	staleChecks := []struct {
		name string
		fn   func() (bool, error)
	}{
		{"check", func() (bool, error) { return st.CheckRAGIndexFence(ctx, first.Fence) }},
		{"heartbeat", func() (bool, error) { return st.HeartbeatRAGIndexTask(ctx, first.Fence, time.Minute) }},
		{"progress", func() (bool, error) {
			return st.UpdateProgressRAGIndexTask(ctx, first.Fence, RAGIndexProgress{Stage: "embed", Current: 1, Total: 2, Unit: "chunks"})
		}},
		{"warning", func() (bool, error) { return st.UpdateWarningRAGIndexTask(ctx, first.Fence, true, 1) }},
		{"retry", func() (bool, error) { return st.RetryRAGIndexTask(ctx, first.Fence, "late", 0) }},
		{"fail", func() (bool, error) { return st.FailRAGIndexTask(ctx, first.Fence, "late") }},
		{"activate", func() (bool, error) {
			return st.ActivateAndFinishRAGIndexTask(ctx, first.Fence, activation, time.Hour)
		}},
	}
	for _, check := range staleChecks {
		if ok, err := check.fn(); err != nil || ok {
			t.Errorf("stale %s = %v, %v; want false, nil", check.name, ok, err)
		}
	}
	if ok, err := st.UpdateProgressRAGIndexTask(ctx, reclaimed.Fence,
		RAGIndexProgress{Stage: "embed", Current: 1, Total: 2, Unit: "chunks"}); err != nil || !ok {
		t.Fatalf("current progress = %v, %v; want true, nil", ok, err)
	}
}

func TestRAGTaskRetryAllocatesAnotherVersionAndPreservesActive(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_claim_retry", 3)
	ctx := context.Background()

	first, err := st.ClaimRAGIndexTask(ctx, "worker-first", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim = %+v, %v", first, err)
	}
	if ok, err := st.RetryRAGIndexTask(ctx, first.Fence, "temporary", 0); err != nil || !ok {
		t.Fatalf("retry = %v, %v", ok, err)
	}
	failed, err := st.GetRAGDocumentVersion(ctx, doc.ID, 1)
	if err != nil || failed.Status != RAGDocumentVersionFailed {
		t.Fatalf("failed attempt = %+v, %v", failed, err)
	}
	second, err := st.ClaimRAGIndexTask(ctx, "worker-second", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("retry claim = %+v, %v", second, err)
	}
	if second.Fence.DocVersion != 2 || second.Fence.ClaimGeneration != 2 || second.Task.RetryCount != 1 {
		t.Fatalf("retry claim fence/task = %+v / %+v", second.Fence, second.Task)
	}
	currentDoc, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || currentDoc.ActiveVersion != 0 {
		t.Fatalf("active version changed on retry: %+v, %v", currentDoc, err)
	}
}

func TestRAGTaskRetryWaitUsesDatabaseTimeGate(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_claim_retry_wait", 3)
	ctx := context.Background()

	first, err := st.ClaimRAGIndexTask(ctx, "worker-first", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim = %+v, %v", first, err)
	}
	if ok, err := st.RetryRAGIndexTask(ctx, first.Fence, "temporary", 10*time.Minute); err != nil || !ok {
		t.Fatalf("retry = %v, %v", ok, err)
	}

	blocked, err := st.ClaimRAGIndexTask(ctx, "worker-too-early", time.Minute)
	if err != nil {
		t.Fatalf("claim before next_run_at: %v", err)
	}
	if blocked != nil {
		t.Fatalf("future next_run_at was ignored: %+v", blocked.Fence)
	}

	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_index_tasks SET next_run_at='2000-01-01 00:00:00' WHERE id=?`,
		first.Fence.TaskID); err != nil {
		t.Fatalf("make retry due: %v", err)
	}
	second, err := st.ClaimRAGIndexTask(ctx, "worker-after-gate", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("claim after next_run_at = %+v, %v", second, err)
	}
	if second.Fence.DocVersion != 2 || second.Fence.ClaimGeneration != 2 ||
		second.Task.RetryCount != 1 || second.Fence.DocID != doc.ID {
		t.Fatalf("retry claim fence/task = %+v / %+v", second.Fence, second.Task)
	}
}

func TestRAGTaskRetryExhaustionFailsAndPreservesActive(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_claim_retry_exhausted", 3)
	ctx := context.Background()

	first, err := st.ClaimRAGIndexTask(ctx, "worker-active", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim active version = %+v, %v", first, err)
	}
	if ok, err := st.ActivateAndFinishRAGIndexTask(ctx, first.Fence,
		RAGIndexActivation{VersionResult: RAGDocumentVersionResult{Status: RAGDocumentVersionDone}},
		time.Hour); err != nil || !ok {
		t.Fatalf("activate v1 = %v, %v", ok, err)
	}

	created, err := st.AdvanceDocumentVersionAndCreateTask(ctx, 1, testRAGVersion(doc.ID, 0))
	if err != nil || created == nil || created.DocVersion != 2 {
		t.Fatalf("advance v2 = %+v, %v", created, err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_index_tasks SET max_retry=0 WHERE id=?`, created.ID); err != nil {
		t.Fatalf("exhaust retry budget: %v", err)
	}
	second, err := st.ClaimRAGIndexTask(ctx, "worker-failing", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("claim v2 = %+v, %v", second, err)
	}
	if ok, err := st.RetryRAGIndexTask(ctx, second.Fence, "transient after exhaustion", time.Minute); err != nil || !ok {
		t.Fatalf("exhausted retry = %v, %v", ok, err)
	}

	task, err := st.GetRAGIndexTask(ctx, second.Fence.TaskID)
	if err != nil || task.Status != "FAILED" || task.FinishedAt == nil || task.NextRunAt != nil {
		t.Fatalf("exhausted task = %+v, %v", task, err)
	}
	failed, err := st.GetRAGDocumentVersion(ctx, doc.ID, 2)
	if err != nil || failed.Status != RAGDocumentVersionFailed {
		t.Fatalf("failed target version = %+v, %v", failed, err)
	}
	currentDoc, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || currentDoc.Version != 2 || currentDoc.ActiveVersion != 1 || currentDoc.Status != "FAILED" {
		t.Fatalf("document after exhausted retry = %+v, %v", currentDoc, err)
	}
	active, err := st.GetRAGDocumentVersion(ctx, doc.ID, 1)
	if err != nil || active.Status != RAGDocumentVersionDone {
		t.Fatalf("previous active version = %+v, %v", active, err)
	}
}

func TestRAGTaskActivateRetiresPreviousVersionAndEnqueuesGC(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, _ := seedRAGTaskDocument(t, st, "doc_claim_activate", 3)
	ctx := context.Background()

	first, err := st.ClaimRAGIndexTask(ctx, "worker-v1", time.Minute)
	if err != nil || first == nil {
		t.Fatalf("claim v1 = %+v, %v", first, err)
	}
	resultV1 := RAGIndexActivation{
		VersionResult: RAGDocumentVersionResult{Status: RAGDocumentVersionDone, ParseArtifactKey: "artifact/v1", PageCount: 1},
		ChunkCount:    2, TokenCount: 20,
	}
	if ok, err := st.ActivateAndFinishRAGIndexTask(ctx, first.Fence, resultV1, time.Hour); err != nil || !ok {
		t.Fatalf("activate v1 = %v, %v", ok, err)
	}
	// Model a pinned legacy active document being rebuilt into the current
	// catalog format; final activation must flip the hydrate contract to v1.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE rag_documents SET index_format_version=0 WHERE id=?`, doc.ID); err != nil {
		t.Fatalf("pin legacy format: %v", err)
	}

	v2snapshot := testRAGVersion(doc.ID, 0)
	v2snapshot.ParseFingerprint = "abababababababababababababababababababababababababababababababab"
	created, err := st.AdvanceDocumentVersionAndCreateTask(ctx, 1, v2snapshot)
	if err != nil || created == nil || created.DocVersion != 2 {
		t.Fatalf("advance v2 = %+v, %v", created, err)
	}
	second, err := st.ClaimRAGIndexTask(ctx, "worker-v2", time.Minute)
	if err != nil || second == nil {
		t.Fatalf("claim v2 = %+v, %v", second, err)
	}
	if ok, err := st.FailRAGIndexTask(ctx, second.Fence, "permanent"); err != nil || !ok {
		t.Fatalf("fail v2 = %v, %v", ok, err)
	}
	afterFailure, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || afterFailure.ActiveVersion != 1 || afterFailure.Version != 2 || afterFailure.Status != "FAILED" {
		t.Fatalf("document after failed target = %+v, %v", afterFailure, err)
	}

	v3snapshot := testRAGVersion(doc.ID, 0)
	created, err = st.AdvanceDocumentVersionAndCreateTask(ctx, 2, v3snapshot)
	if err != nil || created == nil || created.DocVersion != 3 {
		t.Fatalf("advance v3 = %+v, %v", created, err)
	}
	third, err := st.ClaimRAGIndexTask(ctx, "worker-v3", time.Minute)
	if err != nil || third == nil {
		t.Fatalf("claim v3 = %+v, %v", third, err)
	}
	grace := 2 * time.Hour
	resultV3 := RAGIndexActivation{
		VersionResult: RAGDocumentVersionResult{
			Status: RAGDocumentVersionDone, ParseArtifactKey: "artifact/v3", PageCount: 3,
			AssetCount: 4, Degraded: true, WarningCount: 2,
		},
		ChunkCount: 7, TokenCount: 70,
	}
	if ok, err := st.ActivateAndFinishRAGIndexTask(ctx, third.Fence, resultV3, grace); err != nil || !ok {
		t.Fatalf("activate v3 = %v, %v", ok, err)
	}

	currentDoc, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || currentDoc.ActiveVersion != 3 || currentDoc.Version != 3 ||
		currentDoc.Status != "DONE" || currentDoc.IndexFormatVersion != 1 ||
		currentDoc.ChunkCount != 7 || currentDoc.TokenCount != 70 {
		t.Fatalf("activated document = %+v, %v", currentDoc, err)
	}
	v1, err := st.GetRAGDocumentVersion(ctx, doc.ID, 1)
	if err != nil || v1.Status != RAGDocumentVersionRetired {
		t.Fatalf("retired v1 = %+v, %v", v1, err)
	}
	v3, err := st.GetRAGDocumentVersion(ctx, doc.ID, 3)
	if err != nil || v3.Status != RAGDocumentVersionDone || v3.ParseArtifactKey != "artifact/v3" {
		t.Fatalf("done v3 = %+v, %v", v3, err)
	}
	task, err := st.GetRAGIndexTask(ctx, third.Fence.TaskID)
	if err != nil || task.Status != "DONE" || task.FinishedAt == nil {
		t.Fatalf("done task = %+v, %v", task, err)
	}
	gcTasks, err := st.ListRAGIndexGCTasks(ctx, "PENDING", 10)
	if err != nil || len(gcTasks) != 1 || gcTasks[0].RetiredVersion != 1 {
		t.Fatalf("GC tasks = %+v, %v", gcTasks, err)
	}
	if gcTasks[0].NotBefore.Before(gcTasks[0].RetiredAt.Add(grace - time.Second)) {
		t.Fatalf("GC grace too short: retired=%s notBefore=%s", gcTasks[0].RetiredAt, gcTasks[0].NotBefore)
	}
}

func TestRAGTaskAdvanceExpectedVersionIsCASAndSupersedesOldTask(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, oldTaskID := seedRAGTaskDocument(t, st, "doc_claim_advance", 3)
	ctx := context.Background()

	start := make(chan struct{})
	results := make(chan *RAGIndexTaskRecord, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			created, err := st.AdvanceDocumentVersionAndCreateTask(ctx, 1, testRAGVersion(doc.ID, 0))
			results <- created
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	succeeded, conflicted := 0, 0
	for result := range results {
		if result != nil {
			succeeded++
		}
	}
	for err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ErrRAGDocumentVersionConflict):
			conflicted++
		default:
			t.Fatalf("advance error: %v", err)
		}
	}
	if succeeded != 1 || conflicted != 1 {
		t.Fatalf("advance outcomes: succeeded=%d conflicted=%d", succeeded, conflicted)
	}
	currentDoc, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || currentDoc.Version != 2 {
		t.Fatalf("document target = %+v, %v", currentDoc, err)
	}
	oldTask, err := st.GetRAGIndexTask(ctx, oldTaskID)
	if err != nil || oldTask.Status != "SUPERSEDED" {
		t.Fatalf("old task = %+v, %v", oldTask, err)
	}
	oldVersion, err := st.GetRAGDocumentVersion(ctx, doc.ID, 1)
	if err != nil || oldVersion.Status != RAGDocumentVersionSuperseded {
		t.Fatalf("old version = %+v, %v", oldVersion, err)
	}
}

func TestRAGTaskProviderMismatchFencedSupersedeCreatesNewSnapshot(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	doc, oldTaskID := seedRAGTaskDocument(t, st, "doc_claim_provider_change", 3)
	ctx := context.Background()

	claim, err := st.ClaimRAGIndexTask(ctx, "worker-old-provider", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	newSnapshot := testRAGVersion(doc.ID, 0)
	newSnapshot.VisionModel = "vision-v2"
	newSnapshot.VisionProviderFingerprint = "1212121212121212121212121212121212121212121212121212121212121212"
	newSnapshot.ParseFingerprint = "3434343434343434343434343434343434343434343434343434343434343434"

	stale := claim.Fence
	stale.ClaimGeneration++
	if created, ok, err := st.SupersedeRAGIndexTaskAndCreateVersion(ctx, stale, newSnapshot); err != nil || ok || created != nil {
		t.Fatalf("stale provider supersede = %+v, %v, %v; want nil, false, nil", created, ok, err)
	}
	created, ok, err := st.SupersedeRAGIndexTaskAndCreateVersion(ctx, claim.Fence, newSnapshot)
	if err != nil || !ok || created == nil || created.DocVersion != 2 || created.ID == oldTaskID {
		t.Fatalf("provider supersede = %+v, %v, %v", created, ok, err)
	}
	oldTask, err := st.GetRAGIndexTask(ctx, oldTaskID)
	if err != nil || oldTask.Status != "SUPERSEDED" {
		t.Fatalf("old task = %+v, %v", oldTask, err)
	}
	oldVersion, err := st.GetRAGDocumentVersion(ctx, doc.ID, 1)
	if err != nil || oldVersion.Status != RAGDocumentVersionSuperseded {
		t.Fatalf("old version = %+v, %v", oldVersion, err)
	}
	newVersion, err := st.GetRAGDocumentVersion(ctx, doc.ID, 2)
	if err != nil || newVersion.Status != RAGDocumentVersionPending ||
		newVersion.VisionModel != "vision-v2" ||
		newVersion.VisionProviderFingerprint != newSnapshot.VisionProviderFingerprint ||
		newVersion.ParseFingerprint != newSnapshot.ParseFingerprint {
		t.Fatalf("new provider snapshot = %+v, %v", newVersion, err)
	}
	currentDoc, err := st.GetRAGDocument(ctx, doc.ID)
	if err != nil || currentDoc.Version != 2 || currentDoc.ActiveVersion != 0 || currentDoc.Status != "PENDING" {
		t.Fatalf("document after provider supersede = %+v, %v", currentDoc, err)
	}
}
