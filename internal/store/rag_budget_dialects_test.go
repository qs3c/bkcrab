package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type ragBudgetDialectFixture struct {
	primary, competitor *DBStore
	claim               IndexFence
	userID              string
	period              time.Time
	limits              RAGDocumentAILimits
}

func openRAGBudgetDialectStores(t *testing.T, dialect, dsn string) (*DBStore, *DBStore) {
	t.Helper()
	primary, err := NewDBStore(dialect, dsn)
	if err != nil {
		t.Fatalf("open primary %s store: %v", dialect, err)
	}
	t.Cleanup(func() { _ = primary.Close() })
	if err := primary.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate %s: %v", dialect, err)
	}
	competitor, err := NewDBStore(dialect, dsn)
	if err != nil {
		t.Fatalf("open competing %s store: %v", dialect, err)
	}
	t.Cleanup(func() { _ = competitor.Close() })
	return primary, competitor
}

func newRAGBudgetDialectFixture(t *testing.T, primary, competitor *DBStore, suffix string, limits RAGDocumentAILimits) ragBudgetDialectFixture {
	t.Helper()
	ctx := context.Background()
	userID := "u_budget_" + suffix
	docID := "doc_budget_" + suffix
	kbID := "kb_budget_" + suffix
	if err := primary.CreateRAGKB(ctx, &RAGKBRecord{
		ID: kbID, UserID: userID, Name: "budget dialect", EmbedProvider: "system",
		EmbedModel: "embed-v1", EmbedDims: 3, ChunkSize: 512, ChunkOverlap: 64,
		ParseMode: RAGParseModeStandard, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	version := testRAGVersion(docID, 1)
	doc := &RAGDocumentRecord{
		ID: docID, KBID: kbID, FileName: "budget.md", FileType: "md", FileSize: 8,
		ObjectKey: "rag/" + userID + "/" + kbID + "/" + docID + "/budget.md",
		Status:    "PENDING", Version: 1, SourceSHA256: version.SourceSHA256,
		IndexFormatVersion: 1, ProcessingStage: "queued", UploadedAt: time.Now().UTC(),
	}
	taskID, err := primary.CreateRAGDocumentWithVersionAndIndexTask(ctx, doc, version, 3)
	if err != nil {
		t.Fatal(err)
	}
	now, err := primary.ragDBNow(ctx, primary.db)
	if err != nil {
		t.Fatal(err)
	}
	leaseOwner := "worker_" + suffix
	leaseUntil := now.Add(10 * time.Minute)
	result, err := primary.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		status='RUNNING',claim_generation=1,lease_owner=%s,lease_until=%s,
		heartbeat_at=%s,started_at=%s WHERE id=%s`, primary.ph(1), primary.ph(2),
		primary.ph(3), primary.ph(4), primary.ph(5)), leaseOwner, leaseUntil, now, now, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := ragRowsAffected(result); err != nil || !changed {
		t.Fatalf("claim fixture task changed=%v err=%v", changed, err)
	}
	if err := primary.CreateRAGDocumentAITaskBudget(ctx, &RAGDocumentAITaskBudgetRecord{
		TaskID: taskID, UserID: userID, MaxRequests: limits.MaxRequests,
		MaxTokens: limits.MaxTokens, MaxCostMicroUSD: limits.MaxCostMicroUSD,
	}); err != nil {
		t.Fatal(err)
	}
	period := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return ragBudgetDialectFixture{
		primary: primary, competitor: competitor, userID: userID, period: period, limits: limits,
		claim: IndexFence{TaskID: taskID, DocID: docID, DocVersion: 1, ClaimGeneration: 1, LeaseOwner: leaseOwner},
	}
}

func (f ragBudgetDialectFixture) usage(keyDigit, logicalDigit string, input, output, cost int64) *RAGDocumentAIUsageRecord {
	expires := time.Now().UTC().Add(time.Hour)
	keyHash := sha256.Sum256([]byte(f.userID + "|attempt|" + keyDigit))
	logicalHash := sha256.Sum256([]byte(f.userID + "|logical|" + logicalDigit))
	return &RAGDocumentAIUsageRecord{
		IdempotencyKey: hex.EncodeToString(keyHash[:]), LogicalRequestKey: hex.EncodeToString(logicalHash[:]),
		UserID: f.userID, DocID: f.claim.DocID, TaskID: f.claim.TaskID, DocVersion: f.claim.DocVersion,
		ClaimGeneration: f.claim.ClaimGeneration, LeaseOwner: f.claim.LeaseOwner,
		Operation: "vision_page", ProviderFingerprint: strings.Repeat("f", 64), PeriodStartUTC: f.period,
		ReservedInputTokens: input, ReservedOutputTokens: output, EstimatedCostMicroUSD: cost,
		ReservationExpiresAt: &expires,
	}
}

func (f ragBudgetDialectFixture) reclaim(t *testing.T) IndexFence {
	t.Helper()
	ctx := context.Background()
	now, err := f.primary.ragDBNow(ctx, f.primary.db)
	if err != nil {
		t.Fatal(err)
	}
	current := f.claim
	current.ClaimGeneration++
	current.LeaseOwner += "_reclaimed"
	result, err := f.primary.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_index_tasks SET
		status='RUNNING',claim_generation=%s,lease_owner=%s,lease_until=%s,
		heartbeat_at=%s WHERE id=%s`, f.primary.ph(1), f.primary.ph(2), f.primary.ph(3),
		f.primary.ph(4), f.primary.ph(5)), current.ClaimGeneration, current.LeaseOwner,
		now.Add(10*time.Minute), now, current.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := ragRowsAffected(result); err != nil || !changed {
		t.Fatalf("reclaim fixture task changed=%v err=%v", changed, err)
	}
	return current
}

func TestRAGDocumentAIBudgetDialectConcurrencyAndReconciliation(t *testing.T) {
	tests := []struct {
		name, dialect, dsn string
	}{
		{name: "sqlite", dialect: "sqlite", dsn: "file:" + filepath.Join(t.TempDir(), "budget-dialects.db") + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"},
		{name: "postgres", dialect: "postgres", dsn: os.Getenv("BKCRAB_TEST_POSTGRES_DSN")},
		{name: "mysql", dialect: "mysql", dsn: os.Getenv("BKCRAB_TEST_MYSQL_DSN")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.dsn == "" {
				t.Skip("BKCRAB_TEST_" + strings.ToUpper(test.name) + "_DSN is not set")
			}
			primary, competitor := openRAGBudgetDialectStores(t, test.dialect, test.dsn)

			t.Run("two connections compete for final quota", func(t *testing.T) {
				limits := RAGDocumentAILimits{MaxRequests: 1, MaxTokens: 20, MaxCostMicroUSD: 100}
				fixture := newRAGBudgetDialectFixture(t, primary, competitor, test.name+"_last_"+fmt.Sprint(time.Now().UnixNano()), limits)
				requests := []*RAGDocumentAIUsageRecord{
					fixture.usage("1", "a", 5, 5, 100), fixture.usage("2", "b", 5, 5, 100),
				}
				stores := []*DBStore{primary, competitor}
				start := make(chan struct{})
				results := make(chan error, 2)
				var wg sync.WaitGroup
				for index := range stores {
					index := index
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						ok, err := stores[index].ReserveRAGDocumentAIUsage(context.Background(), fixture.claim, requests[index], limits)
						if err == nil && !ok {
							err = errors.New("reservation unexpectedly returned false,nil")
						}
						results <- err
					}()
				}
				close(start)
				wg.Wait()
				close(results)
				success, limited := 0, 0
				for err := range results {
					switch {
					case err == nil:
						success++
					case errors.Is(err, ErrRAGDocumentAIBudgetExceeded):
						limited++
					default:
						t.Fatalf("unexpected reserve error: %v", err)
					}
				}
				if success != 1 || limited != 1 {
					t.Fatalf("reserve outcomes success=%d limited=%d", success, limited)
				}
			})

			t.Run("reclaim and crash reconciliation", func(t *testing.T) {
				limits := RAGDocumentAILimits{MaxRequests: 5, MaxTokens: 100, MaxCostMicroUSD: 1_000}
				fixture := newRAGBudgetDialectFixture(t, primary, competitor, test.name+"_reconcile_"+fmt.Sprint(time.Now().UnixNano()), limits)
				staleSend := fixture.usage("3", "c", 5, 5, 100)
				abandonedReserved := fixture.usage("4", "d", 5, 5, 100)
				expired := time.Now().UTC().Add(-time.Hour)
				abandonedReserved.ReservationExpiresAt = &expired
				sent := fixture.usage("5", "e", 6, 4, 120)
				for _, usage := range []*RAGDocumentAIUsageRecord{staleSend, abandonedReserved, sent} {
					if ok, err := primary.ReserveRAGDocumentAIUsage(context.Background(), fixture.claim, usage, limits); err != nil || !ok {
						t.Fatalf("reserve %s: ok=%v err=%v", usage.IdempotencyKey[:1], ok, err)
					}
				}
				if ok, err := primary.MarkSentRAGDocumentAIUsage(context.Background(), sent.IdempotencyKey, fixture.claim); err != nil || !ok {
					t.Fatalf("mark sent: ok=%v err=%v", ok, err)
				}
				current := fixture.reclaim(t)
				if ok, err := competitor.MarkSentRAGDocumentAIUsage(context.Background(), staleSend.IdempotencyKey, fixture.claim); err != nil || ok {
					t.Fatalf("stale MarkSent: ok=%v err=%v", ok, err)
				}
				future := time.Now().UTC().Add(24 * time.Hour)
				if count, err := competitor.ReconcileRAGDocumentAIUsage(context.Background(), future, future, 20); err != nil || count < 2 {
					t.Fatalf("reconcile count=%d err=%v", count, err)
				}
				reservedResult, err := primary.GetRAGDocumentAIUsage(context.Background(), abandonedReserved.IdempotencyKey)
				if err != nil || reservedResult.State != RAGDocumentAIUsageReleased {
					t.Fatalf("reconciled RESERVED usage=%+v err=%v", reservedResult, err)
				}
				got, err := primary.GetRAGDocumentAIUsage(context.Background(), sent.IdempotencyKey)
				if err != nil || got.State != RAGDocumentAIUsageCommitted || !got.UsageEstimated {
					t.Fatalf("reconciled SENT usage=%+v err=%v", got, err)
				}
				currentUsage := fixture.usage("6", "f", 5, 5, 100)
				currentUsage.ClaimGeneration = current.ClaimGeneration
				currentUsage.LeaseOwner = current.LeaseOwner
				if ok, err := competitor.ReserveRAGDocumentAIUsage(context.Background(), current, currentUsage, limits); err != nil || !ok {
					t.Fatalf("current fence reserve after reconciliation: ok=%v err=%v", ok, err)
				}
			})
		})
	}
}
