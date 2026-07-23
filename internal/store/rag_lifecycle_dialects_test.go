package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type ragLifecycleDialectFixture struct {
	primary, competitor *DBStore
	userID, kbID        string
	docIDs              []string
}

var ragLifecycleDialectSequence atomic.Uint64

func newRAGLifecycleDialectFixture(
	t *testing.T,
	primary, competitor *DBStore,
	dialect, purpose string,
) *ragLifecycleDialectFixture {
	t.Helper()
	suffix := fmt.Sprintf("%s_%s_%x_%x", dialect, purpose,
		time.Now().UTC().UnixNano(), ragLifecycleDialectSequence.Add(1))
	fixture := &ragLifecycleDialectFixture{
		primary: primary, competitor: competitor,
		userID: "u_lifecycle_" + suffix,
		kbID:   "kb_lifecycle_" + suffix,
	}
	now := time.Now().UTC()
	if err := primary.CreateUser(context.Background(), &UserRecord{
		ID: fixture.userID, Username: fixture.userID,
		Email: fixture.userID + "@example.invalid", DisplayName: fixture.userID,
		Role: "user", Status: "active", AgentQuota: -1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create %s lifecycle user: %v", dialect, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := tombstoneAndDeleteRAGKBForTest(ctx, primary, fixture.kbID); err != nil && !errors.Is(err, ErrNotFound) {
			t.Errorf("clean up %s lifecycle KB: %v", dialect, err)
		}
		if err := primary.DeleteUser(ctx, fixture.userID); err != nil && !errors.Is(err, ErrNotFound) {
			t.Errorf("clean up %s lifecycle user: %v", dialect, err)
		}
	})
	if err := primary.CreateRAGKB(context.Background(), &RAGKBRecord{
		ID: fixture.kbID, UserID: fixture.userID, Name: "lifecycle dialect",
		EmbedProvider: "system", EmbedModel: "embed-v1", EmbedDims: 3,
		ChunkSize: 512, ChunkOverlap: 64, ParseMode: RAGParseModeAuto,
		EnrichmentEnabled: true, Status: "active",
	}); err != nil {
		t.Fatalf("create %s lifecycle KB: %v", dialect, err)
	}
	return fixture
}

func (f *ragLifecycleDialectFixture) advancedDocument(t *testing.T, label string) (*RAGDocumentRecord, *RAGDocumentVersionRecord) {
	t.Helper()
	docID := "doc_" + strings.TrimPrefix(f.kbID, "kb_") + "_" + label
	f.docIDs = append(f.docIDs, docID)
	version := testRAGVersion(docID, 1)
	version.ParseMode = RAGParseModeAuto
	version.EnrichmentEnabled = true
	doc := &RAGDocumentRecord{
		ID: docID, KBID: f.kbID, FileName: label + ".pdf", FileType: "pdf",
		FileSize: 12, ObjectKey: "rag/" + f.userID + "/" + f.kbID + "/" + docID + "/source.pdf",
		Status: "PENDING", Version: 1, SourceSHA256: version.SourceSHA256,
		IndexFormatVersion: 1, ProcessingStage: "queued", UploadedAt: time.Now().UTC(),
	}
	return doc, version
}

func TestRAGLifecycleDialectConcurrency(t *testing.T) {
	tests := []struct {
		name, dialect, dsn string
	}{
		{name: "postgres", dialect: "postgres", dsn: os.Getenv("BKCRAB_TEST_POSTGRES_DSN")},
		{name: "mysql", dialect: "mysql", dsn: os.Getenv("BKCRAB_TEST_MYSQL_DSN")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.dsn == "" {
				t.Skip("BKCRAB_TEST_" + strings.ToUpper(test.name) + "_DSN is not set")
			}
			primary, competitor := openRAGBudgetDialectStores(t, test.dialect, test.dsn)

			t.Run("progress update uses a portable fenced transaction", func(t *testing.T) {
				fixture := newRAGLifecycleDialectFixture(t, primary, competitor, test.dialect, "progress")
				doc, version := fixture.advancedDocument(t, "progress")
				ctx := context.Background()
				if _, err := primary.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, version, 3); err != nil {
					t.Fatalf("create progress fixture: %v", err)
				}
				claim, err := primary.ClaimRAGIndexTask(ctx, "lifecycle-progress-worker", time.Minute)
				if err != nil || claim == nil || claim.Fence.DocID != doc.ID {
					t.Fatalf("progress claim=%+v err=%v", claim, err)
				}
				progress := RAGIndexProgress{Stage: "parsing", Current: 1, Total: 2, Unit: "pages"}
				for attempt := 0; attempt < 2; attempt++ {
					if ok, err := primary.UpdateProgressRAGIndexTask(ctx, claim.Fence, progress); err != nil || !ok {
						t.Fatalf("progress update %d ok=%v err=%v", attempt+1, ok, err)
					}
				}
				current, err := primary.GetRAGDocument(ctx, doc.ID)
				if err != nil || current.ProcessingStage != progress.Stage ||
					current.ProgressCurrent != progress.Current || current.ProgressTotal != progress.Total ||
					current.ProgressUnit != progress.Unit {
					t.Fatalf("persisted progress=%+v err=%v", current, err)
				}
				if ok, err := primary.FailRAGIndexTask(ctx, claim.Fence, "progress regression completed"); err != nil || !ok {
					t.Fatalf("finish progress fixture ok=%v err=%v", ok, err)
				}
			})

			t.Run("uncommitted user tombstone blocks KB provisioning", func(t *testing.T) {
				suffix := fmt.Sprintf("%s_provision_%x", test.dialect, ragLifecycleDialectSequence.Add(1))
				userID := "u_" + suffix
				kbID := "kb_" + suffix
				now := time.Now().UTC()
				if err := primary.CreateUser(context.Background(), &UserRecord{
					ID: userID, Username: userID, Email: userID + "@example.invalid",
					Role: "user", Status: "active", AgentQuota: -1, CreatedAt: now, UpdatedAt: now,
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_ = tombstoneAndDeleteRAGKBForTest(ctx, primary, kbID)
					_ = primary.DeleteUser(ctx, userID)
				})
				deleteTx, err := primary.db.BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer deleteTx.Rollback()
				if _, err := deleteTx.ExecContext(context.Background(), fmt.Sprintf(
					`UPDATE users SET status='deleting',updated_at=%s WHERE id=%s`,
					primary.ragNowExpr(), primary.ph(1)), userID); err != nil {
					t.Fatal(err)
				}
				result := make(chan error, 1)
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_, beginErr := competitor.BeginRAGKBProvisioning(ctx,
						testRAGProvisioningKB(userID, kbID), "dialect-provision-worker", time.Minute, 20)
					result <- beginErr
				}()
				select {
				case err := <-result:
					t.Fatalf("provisioning did not wait for user tombstone: %v", err)
				case <-time.After(200 * time.Millisecond):
				}
				if err := deleteTx.Commit(); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-result:
					if !errors.Is(err, ErrRAGLifecycleInactive) {
						t.Fatalf("provisioning after tombstone err=%v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("provisioning remained blocked after tombstone commit")
				}
				if _, err := primary.GetRAGKB(context.Background(), kbID); !errors.Is(err, ErrNotFound) {
					t.Fatalf("failed provisioning published KB: %v", err)
				}
			})

			t.Run("KB tombstone revokes provisioning lease before finalizer", func(t *testing.T) {
				suffix := fmt.Sprintf("%s_provision_delete_%x", test.dialect, ragLifecycleDialectSequence.Add(1))
				userID := "u_" + suffix
				kbID := "kb_" + suffix
				now := time.Now().UTC()
				if err := primary.CreateUser(context.Background(), &UserRecord{
					ID: userID, Username: userID, Email: userID + "@example.invalid",
					Role: "user", Status: "active", AgentQuota: -1, CreatedAt: now, UpdatedAt: now,
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					_ = tombstoneAndDeleteRAGKBForTest(ctx, primary, kbID)
					_ = primary.DeleteUser(ctx, userID)
				})
				fence, err := primary.BeginRAGKBProvisioning(context.Background(),
					testRAGProvisioningKB(userID, kbID), "dialect-delete-worker", time.Minute, 20)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := competitor.MarkRAGKBDeleting(context.Background(), kbID); err != nil {
					t.Fatal(err)
				}
				if ok, err := primary.HeartbeatRAGKBProvisioning(context.Background(), *fence, time.Minute); err != nil || ok {
					t.Fatalf("heartbeat crossed tombstone ok=%v err=%v", ok, err)
				}
				if err := competitor.DeleteRAGKB(context.Background(), kbID); !errors.Is(err, ErrRAGKBProvisioningActive) {
					t.Fatalf("finalizer crossed live provision lease: %v", err)
				}
				if _, ok, err := primary.AbortRAGKBProvisioning(context.Background(), *fence); err != nil || !ok {
					t.Fatalf("abort after tombstone ok=%v err=%v", ok, err)
				}
				if err := competitor.DeleteRAGKB(context.Background(), kbID); err != nil {
					t.Fatalf("finalize quiesced provisioning: %v", err)
				}
			})

			t.Run("uncommitted user tombstone blocks claim and fails closed", func(t *testing.T) {
				fixture := newRAGLifecycleDialectFixture(t, primary, competitor, test.dialect, "claim")
				doc, version := fixture.advancedDocument(t, "claim")
				if _, err := primary.CreateRAGDocumentWithVersionAndIndexTask(
					context.Background(), doc, version, 3,
				); err != nil {
					t.Fatalf("create claim fixture: %v", err)
				}

				deleteTx, err := primary.db.BeginTx(context.Background(), nil)
				if err != nil {
					t.Fatalf("begin user tombstone transaction: %v", err)
				}
				defer deleteTx.Rollback()
				if _, err := deleteTx.ExecContext(context.Background(), fmt.Sprintf(
					`UPDATE users SET status='deleting',updated_at=%s WHERE id=%s`,
					primary.ragNowExpr(), primary.ph(1)), fixture.userID); err != nil {
					t.Fatalf("write uncommitted user tombstone: %v", err)
				}
				// This is the same transaction-external routing read used by the
				// claim path. It must be able to finish while the user tombstone is
				// uncommitted; only the later user-row lock is the serialization gate.
				route, err := competitor.ragOwnershipRoute(context.Background(), doc.ID)
				if err != nil || route.KBID != fixture.kbID || route.UserID != fixture.userID {
					t.Fatalf("read claim route before user lock: route=%+v err=%v", route, err)
				}

				claimResult := make(chan struct {
					claim *RAGIndexClaim
					err   error
				}, 1)
				claimStarted := make(chan struct{})
				go func() {
					close(claimStarted)
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					claim, claimErr := competitor.ClaimRAGIndexTask(ctx, "lifecycle-dialect-worker", time.Minute)
					claimResult <- struct {
						claim *RAGIndexClaim
						err   error
					}{claim: claim, err: claimErr}
				}()
				<-claimStarted
				select {
				case result := <-claimResult:
					t.Fatalf("claim did not wait for user tombstone transaction: claim=%+v err=%v", result.claim, result.err)
				case <-time.After(200 * time.Millisecond):
				}

				if err := deleteTx.Commit(); err != nil {
					t.Fatalf("commit user tombstone: %v", err)
				}
				select {
				case result := <-claimResult:
					if result.err != nil || result.claim != nil {
						t.Fatalf("claim after committed tombstone was not fail-closed: claim=%+v err=%v", result.claim, result.err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("claim remained blocked after user tombstone committed")
				}
				taskCount := 0
				if err := primary.db.QueryRowContext(context.Background(), fmt.Sprintf(
					`SELECT COUNT(*) FROM rag_index_tasks WHERE doc_id=%s AND status IN ('PENDING','RUNNING')`,
					primary.ph(1)), doc.ID).Scan(&taskCount); err != nil {
					t.Fatalf("count runnable tasks after tombstone: %v", err)
				}
				if taskCount != 0 {
					t.Fatalf("committed tombstone left %d runnable task(s)", taskCount)
				}
			})

			t.Run("two connections admit only one final pending slot", func(t *testing.T) {
				fixture := newRAGLifecycleDialectFixture(t, primary, competitor, test.dialect, "pending")
				policy := RAGAdvancedEnqueuePolicy{
					UserID: fixture.userID, MaxPendingTasks: 1, MinReindexInterval: time.Hour,
				}
				stores := []*DBStore{primary, competitor}
				start := make(chan struct{})
				results := make(chan error, len(stores))
				for i, st := range stores {
					doc, version := fixture.advancedDocument(t, fmt.Sprintf("pending_%d", i))
					go func(st *DBStore, doc *RAGDocumentRecord, version *RAGDocumentVersionRecord) {
						<-start
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						_, enqueueErr := st.CreateRAGDocumentWithVersionAndIndexTaskPolicy(ctx, doc, version, 3, policy)
						results <- enqueueErr
					}(st, doc, version)
				}
				close(start)
				succeeded, limited := 0, 0
				for range stores {
					select {
					case err := <-results:
						switch {
						case err == nil:
							succeeded++
						case errors.Is(err, ErrRAGAdvancedPendingLimit):
							limited++
						default:
							t.Fatalf("unexpected concurrent pending admission error: %v", err)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("timed out waiting for concurrent pending admission")
					}
				}
				if succeeded != 1 || limited != 1 {
					t.Fatalf("pending admission outcomes success=%d limited=%d", succeeded, limited)
				}
			})

			t.Run("two connections cannot both pass minimum reindex interval", func(t *testing.T) {
				fixture := newRAGLifecycleDialectFixture(t, primary, competitor, test.dialect, "reindex")
				doc, version := fixture.advancedDocument(t, "reindex")
				if _, err := primary.CreateRAGDocumentWithVersionAndIndexTask(
					context.Background(), doc, version, 3,
				); err != nil {
					t.Fatalf("create reindex fixture: %v", err)
				}
				if _, err := primary.db.ExecContext(context.Background(), fmt.Sprintf(
					`UPDATE rag_index_tasks SET status='DONE',created_at=%s WHERE doc_id=%s`,
					primary.ph(1), primary.ph(2)), time.Now().UTC().Add(-2*time.Hour), doc.ID); err != nil {
					t.Fatalf("age initial reindex task: %v", err)
				}
				reindexVersion := *version
				reindexVersion.DocVersion = 2
				reindexVersion.CreatedAt = time.Time{}
				reindexVersion.UpdatedAt = time.Time{}
				if err := primary.CreateRAGDocumentVersion(context.Background(), &reindexVersion); err != nil {
					t.Fatalf("create candidate reindex version: %v", err)
				}
				policy := RAGAdvancedEnqueuePolicy{
					UserID: fixture.userID, MaxPendingTasks: 10, MinReindexInterval: time.Hour,
				}
				stores := []*DBStore{primary, competitor}
				start := make(chan struct{})
				results := make(chan error, len(stores))
				for _, st := range stores {
					go func(st *DBStore) {
						<-start
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						tx, txErr := st.db.BeginTx(ctx, nil)
						if txErr == nil {
							defer tx.Rollback()
							txErr = st.enforceRAGAdvancedEnqueuePolicyTx(
								ctx, tx, fixture.kbID, doc.ID, &reindexVersion, &policy, true,
							)
						}
						if txErr == nil {
							_, txErr = st.createRAGIndexTaskForVersion(
								ctx, tx, doc.ID, reindexVersion.DocVersion, 3,
							)
						}
						if txErr == nil {
							txErr = tx.Commit()
						}
						results <- txErr
					}(st)
				}
				close(start)
				succeeded, limited := 0, 0
				for range stores {
					select {
					case err := <-results:
						switch {
						case err == nil:
							succeeded++
						case errors.Is(err, ErrRAGAdvancedReindexRateLimit):
							limited++
						default:
							t.Fatalf("unexpected concurrent reindex admission error: %v", err)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("timed out waiting for concurrent reindex admission")
					}
				}
				if succeeded != 1 || limited != 1 {
					t.Fatalf("reindex admission outcomes success=%d limited=%d", succeeded, limited)
				}
			})
		})
	}
}

func TestRAGLifecycleCatalogSemanticsOnPostgresAndMySQL(t *testing.T) {
	tests := []struct {
		name, dialect, dsn string
	}{
		{name: "postgres", dialect: "postgres", dsn: os.Getenv("BKCRAB_TEST_POSTGRES_DSN")},
		{name: "mysql", dialect: "mysql", dsn: os.Getenv("BKCRAB_TEST_MYSQL_DSN")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.dsn == "" {
				t.Skip("BKCRAB_TEST_" + strings.ToUpper(test.name) + "_DSN is not set")
			}
			primary, competitor := openRAGBudgetDialectStores(t, test.dialect, test.dsn)
			fixture := newRAGLifecycleDialectFixture(t, primary, competitor, test.dialect, "catalog")
			doc, version := fixture.advancedDocument(t, "catalog")
			ctx := context.Background()
			if _, err := primary.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, version, 3); err != nil {
				t.Fatal(err)
			}
			claim, err := primary.ClaimRAGIndexTask(ctx, "catalog-dialect-v1", time.Minute)
			if err != nil || claim == nil || claim.Fence.DocID != doc.ID {
				t.Fatalf("v1 claim=%+v err=%v", claim, err)
			}
			artifactPrefix := fmt.Sprintf("rag/%s/%s/%s/artifacts/%s/",
				fixture.userID, fixture.kbID, doc.ID, claim.Version.ParseFingerprint)
			artifactGenerationPrefix := fmt.Sprintf("%sversions/%d/", artifactPrefix, claim.Fence.DocVersion)
			artifactKey := artifactGenerationPrefix + "parsed.json"
			if ok, err := primary.RecordRAGDocumentParseArtifact(ctx, claim.Fence, artifactKey); err != nil || !ok {
				t.Fatalf("record artifact ok=%v err=%v", ok, err)
			}
			for _, item := range []struct{ kind, key string }{
				{RAGObjectKindNormalized, artifactGenerationPrefix + "normalized.md"},
				{RAGObjectKindParsedArtifact, artifactKey},
			} {
				fence, err := primary.BeginRAGObjectWrite(ctx, RAGObjectWriteRequest{
					UserID: fixture.userID, KBID: fixture.kbID, DocID: doc.ID,
					ObjectKind: item.kind, ObjectKey: item.key, ReferenceKey: artifactKey,
				})
				if err != nil {
					t.Fatal(err)
				}
				if ready, err := primary.MarkRAGObjectWriteReady(ctx, *fence); err != nil || !ready {
					t.Fatalf("ready=%v err=%v", ready, err)
				}
			}
			if ok, err := primary.ActivateAndFinishRAGIndexTask(ctx, claim.Fence, RAGIndexActivation{
				VersionResult: RAGDocumentVersionResult{ParseArtifactKey: artifactKey},
			}, 0); err != nil || !ok {
				t.Fatalf("activate v1 ok=%v err=%v", ok, err)
			}
			var staged int
			if err := primary.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*)
				FROM rag_object_write_staging WHERE doc_id=%s AND status='PUBLISHED'`, primary.ph(1)), doc.ID).
				Scan(&staged); err != nil || staged != 2 {
				t.Fatalf("activation staging rows=%d err=%v", staged, err)
			}

			parseFingerprint := strings.Repeat("1", 64)
			indexFingerprint := strings.Repeat("2", 64)
			pageKey := strings.Repeat("a", 64)
			enrichKey := strings.Repeat("b", 64)
			for _, record := range []RAGCacheObjectRecord{
				{DocID: doc.ID, CacheKind: RAGCacheKindPage, CacheKey: pageKey,
					ObjectKey: fmt.Sprintf("rag/%s/%s/%s/cache/pages/%s.json",
						fixture.userID, fixture.kbID, doc.ID, pageKey),
					FingerprintKind: RAGCacheFingerprintParse, Fingerprint: parseFingerprint},
				{DocID: doc.ID, CacheKind: RAGCacheKindEnrich, CacheKey: enrichKey,
					ObjectKey: fmt.Sprintf("rag/%s/%s/%s/cache/enrich/%s.json",
						fixture.userID, fixture.kbID, doc.ID, enrichKey),
					FingerprintKind: RAGCacheFingerprintIndex, Fingerprint: indexFingerprint},
			} {
				if err := primary.RegisterRAGCacheObject(ctx, record); err != nil {
					t.Fatal(err)
				}
			}
			dbNow, err := primary.ragDBNow(ctx, primary.db)
			if err != nil {
				t.Fatal(err)
			}
			for fingerprint, updatedAt := range map[string]time.Time{
				parseFingerprint: dbNow.Add(-time.Minute),
				indexFingerprint: dbNow.Add(-2 * time.Minute),
			} {
				if _, err := primary.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_cache_object_fingerprints
					SET updated_at=%s WHERE doc_id=%s AND fingerprint=%s`, primary.ph(1),
					primary.ph(2), primary.ph(3)), updatedAt, doc.ID, fingerprint); err != nil {
					t.Fatal(err)
				}
			}
			maintenance, err := primary.ClaimRAGDocumentMaintenance(ctx, doc.ID, "catalog-prune", time.Minute)
			if err != nil || maintenance == nil {
				t.Fatalf("maintenance=%+v err=%v", maintenance, err)
			}
			cacheCandidates, err := primary.PruneRAGCacheCatalogAndListCleanupCandidates(
				ctx, *maintenance, time.Hour, 1, 10,
			)
			if err != nil || len(cacheCandidates) != 1 || cacheCandidates[0].CacheKey != enrichKey {
				t.Fatalf("cache candidates=%+v err=%v", cacheCandidates, err)
			}
			if deleted, err := primary.DeleteRAGCacheObjectWithMaintenance(
				ctx, *maintenance, cacheCandidates[0],
			); err != nil || !deleted {
				t.Fatalf("delete cache=%v err=%v", deleted, err)
			}
			if released, err := primary.ReleaseRAGDocumentMaintenance(ctx, *maintenance); err != nil || !released {
				t.Fatalf("release maintenance=%v err=%v", released, err)
			}

			next := *version
			next.DocVersion = 0
			next.CreatedAt = time.Time{}
			next.UpdatedAt = time.Time{}
			if _, err := primary.AdvanceDocumentVersionAndCreateTask(ctx, 1, &next); err != nil {
				t.Fatal(err)
			}
			claim2, err := primary.ClaimRAGIndexTask(ctx, "catalog-dialect-v2", time.Minute)
			if err != nil || claim2 == nil || claim2.Fence.DocID != doc.ID {
				t.Fatalf("v2 claim=%+v err=%v", claim2, err)
			}
			artifactKey2 := artifactPrefix + "v2/parsed.json"
			if ok, err := primary.ActivateAndFinishRAGIndexTask(ctx, claim2.Fence, RAGIndexActivation{
				VersionResult: RAGDocumentVersionResult{ParseArtifactKey: artifactKey2},
			}, 0); err != nil || !ok {
				t.Fatalf("activate v2 ok=%v err=%v", ok, err)
			}
			if err := primary.PutRAGChunks(ctx, []RAGChunkRecord{
				{KBID: fixture.kbID, DocID: doc.ID, DocVersion: 1, ChunkIndex: 0, RawContent: "v1", SearchContent: "v1"},
				{KBID: fixture.kbID, DocID: doc.ID, DocVersion: 2, ChunkIndex: 0, RawContent: "v2", SearchContent: "v2"},
			}); err != nil {
				t.Fatal(err)
			}
			gc, err := primary.ClaimRAGIndexGCTask(ctx, "catalog-dialect-gc", time.Minute)
			if err != nil || gc == nil || gc.Fence.RetiredVersion != 1 {
				t.Fatalf("GC claim=%+v err=%v", gc, err)
			}
			if ok, err := primary.FinishRAGIndexGCTask(ctx, gc.Fence); err != nil || !ok {
				t.Fatalf("finish GC ok=%v err=%v", ok, err)
			}
			oldChunks, err := primary.ListRAGChunksByDocumentVersion(ctx, doc.ID, 1)
			if err != nil || len(oldChunks) != 0 {
				t.Fatalf("old chunks=%+v err=%v", oldChunks, err)
			}
			currentChunks, err := primary.ListRAGChunksByDocumentVersion(ctx, doc.ID, 2)
			if err != nil || len(currentChunks) != 1 {
				t.Fatalf("current chunks=%+v err=%v", currentChunks, err)
			}
			if err := primary.DeleteRAGDocument(ctx, doc.ID); !errors.Is(err, ErrRAGLifecycleInactive) {
				t.Fatalf("active finalizer err=%v", err)
			}
			if err := tombstoneAndDeleteRAGDocumentForTest(ctx, primary, doc.ID); err != nil {
				t.Fatalf("tombstoned finalizer: %v", err)
			}
		})
	}
}
