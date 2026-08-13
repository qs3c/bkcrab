package vision

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/store"
)

type fakeBudgetLedger struct {
	mu            sync.Mutex
	createCalls   int
	fencedCreates int
	createFence   store.IndexFence
	reserve       *store.RAGDocumentAIUsageRecord
	reserveFence  store.IndexFence
	markFence     store.IndexFence
	commits       int
	releases      int
	markErr       error
	commitErr     error
	releaseErr    error
}

func (f *fakeBudgetLedger) CreateRAGDocumentAITaskBudget(_ context.Context, _ *store.RAGDocumentAITaskBudgetRecord) error {
	f.mu.Lock()
	f.createCalls++
	f.mu.Unlock()
	return nil
}
func (f *fakeBudgetLedger) CreateRAGDocumentAITaskBudgetForIndex(
	_ context.Context,
	fence store.IndexFence,
	_ *store.RAGDocumentAITaskBudgetRecord,
) error {
	f.mu.Lock()
	f.fencedCreates++
	f.createFence = fence
	f.mu.Unlock()
	return nil
}
func (f *fakeBudgetLedger) ReserveRAGDocumentAIUsage(_ context.Context, fence store.IndexFence, usage *store.RAGDocumentAIUsageRecord, _ store.RAGDocumentAILimits) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	copyUsage := *usage
	f.reserve = &copyUsage
	f.reserveFence = fence
	return true, nil
}
func (f *fakeBudgetLedger) MarkSentRAGDocumentAIUsage(_ context.Context, _ string, fence store.IndexFence) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.markFence = fence
	return f.markErr == nil, f.markErr
}
func (f *fakeBudgetLedger) CommitRAGDocumentAIUsage(_ context.Context, _ string, _, _, _ int64, _ bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits++
	return f.commitErr == nil, f.commitErr
}
func (f *fakeBudgetLedger) ReleaseRAGDocumentAIUsage(_ context.Context, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.releases++
	return f.releaseErr == nil, f.releaseErr
}
func (f *fakeBudgetLedger) GetRAGDocumentAIUsage(context.Context, string) (*store.RAGDocumentAIUsageRecord, error) {
	return nil, store.ErrNotFound
}

func newFakeTaskBudget(ledger BudgetLedger) *TaskDocumentAIBudget {
	fence := store.IndexFence{TaskID: 11, DocID: "doc_1", DocVersion: 1, ClaimGeneration: 1, LeaseOwner: "worker"}
	return newFakeTaskBudgetForFence(ledger, fence)
}

func newFakeTaskBudgetForFence(ledger BudgetLedger, fence store.IndexFence) *TaskDocumentAIBudget {
	return NewTaskDocumentAIBudget(ledger, TaskBudgetConfig{
		Fence:          fence,
		UserID:         "u_1",
		TaskLimits:     store.RAGDocumentAILimits{MaxRequests: 10, MaxTokens: 10_000, MaxCostMicroUSD: 1_000_000},
		UserLimits:     store.RAGDocumentAILimits{MaxRequests: 100, MaxTokens: 100_000, MaxCostMicroUSD: 10_000_000},
		ReservationTTL: time.Minute,
	})
}

func TestTaskDocumentAIBudgetUsesFencedDurableTransitions(t *testing.T) {
	ctx := context.Background()
	ledger := &fakeBudgetLedger{}
	fence := store.IndexFence{TaskID: 7, DocID: "doc_1", DocVersion: 2, ClaimGeneration: 3, LeaseOwner: "worker"}
	budget := newFakeTaskBudgetForFence(ledger, fence)
	reservation, err := budget.Reserve(ctx, fence, AttemptRequest{
		LogicalRequestKey: "logical", Operation: OperationPage,
		ProviderFingerprint: "provider", Attempt: 0,
		InputTokens: 20, OutputTokens: 30, EstimatedCostMicroUSD: 50,
	})
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if err := reservation.MarkSent(ctx, fence); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if err := reservation.Commit(ctx, Usage{InputTokens: 18, OutputTokens: 4}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.createCalls != 1 || ledger.commits != 1 || ledger.releases != 0 {
		t.Fatalf("ledger calls create=%d commit=%d release=%d", ledger.createCalls, ledger.commits, ledger.releases)
	}
	if ledger.reserve == nil || ledger.reserve.DocVersion != fence.DocVersion ||
		ledger.reserve.ClaimGeneration != fence.ClaimGeneration || ledger.reserveFence != fence || ledger.markFence != fence {
		t.Fatalf("fence not propagated: usage=%+v reserve=%+v mark=%+v", ledger.reserve, ledger.reserveFence, ledger.markFence)
	}
	if ledger.reserve.State != "" || ledger.reserve.PeriodStartUTC.Location() != time.UTC {
		t.Fatalf("usage facade leaked state or non-UTC period: %+v", ledger.reserve)
	}
}

type budgetLedgerWithoutFence struct{ BudgetLedger }

func TestTaskDocumentAIBudgetFairCreateRequiresFencedLedger(t *testing.T) {
	fence := store.IndexFence{
		TaskID: 17, DocID: "doc_fair", DocVersion: 2, ClaimGeneration: 3,
		LeaseOwner: "fair-worker", ExpectedWriterFingerprint: strings.Repeat("a", 64),
	}
	t.Run("uses live fence entry point", func(t *testing.T) {
		ledger := &fakeBudgetLedger{}
		budget := newFakeTaskBudgetForFence(ledger, fence)
		if _, err := budget.Reserve(context.Background(), fence, AttemptRequest{
			LogicalRequestKey: "fair-logical", Operation: OperationPage,
			ProviderFingerprint: "provider", InputTokens: 1,
		}); err != nil {
			t.Fatal(err)
		}
		ledger.mu.Lock()
		defer ledger.mu.Unlock()
		if ledger.fencedCreates != 1 || ledger.createCalls != 0 || ledger.createFence != fence {
			t.Fatalf("fenced=%d legacy=%d fence=%+v",
				ledger.fencedCreates, ledger.createCalls, ledger.createFence)
		}
	})

	t.Run("no legacy fallback", func(t *testing.T) {
		underlying := &fakeBudgetLedger{}
		ledger := &budgetLedgerWithoutFence{BudgetLedger: underlying}
		budget := newFakeTaskBudgetForFence(ledger, fence)
		reservation, err := budget.Reserve(context.Background(), fence, AttemptRequest{
			LogicalRequestKey: "fair-no-fallback", Operation: OperationPage,
			ProviderFingerprint: "provider", InputTokens: 1,
		})
		if reservation != nil || !errors.Is(err, store.ErrRAGDocumentAIInvalidFence) {
			t.Fatalf("reservation=%+v err=%v", reservation, err)
		}
		underlying.mu.Lock()
		defer underlying.mu.Unlock()
		if underlying.createCalls != 0 || underlying.reserve != nil {
			t.Fatalf("legacy fallback create=%d reserve=%+v",
				underlying.createCalls, underlying.reserve)
		}
	})
}

func TestTaskDocumentAIBudgetReleaseBeforeSendAndAttemptKeysDiffer(t *testing.T) {
	ledger := &fakeBudgetLedger{}
	fence := store.IndexFence{TaskID: 9, DocID: "doc_2", DocVersion: 1, ClaimGeneration: 1, LeaseOwner: "worker"}
	budget := newFakeTaskBudgetForFence(ledger, fence)
	first, err := budget.Reserve(context.Background(), fence, AttemptRequest{
		LogicalRequestKey: "logical", Operation: OperationImage, ProviderFingerprint: "provider",
		InputTokens: 1, OutputTokens: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if first.Key() == AttemptKey(fence, "logical", OperationImage, 1) {
		t.Fatal("attempt ordinal must change the durable idempotency key")
	}
}
