package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

type ragFairQueueBudgetLedgerSurface interface {
	CreateRAGDocumentAITaskBudget(context.Context, *RAGDocumentAITaskBudgetRecord) error
	CreateRAGDocumentAITaskBudgetForIndex(context.Context, IndexFence, *RAGDocumentAITaskBudgetRecord) error
	GetRAGDocumentAIUsage(context.Context, string) (*RAGDocumentAIUsageRecord, error)
	ReserveRAGDocumentAIUsage(context.Context, IndexFence, *RAGDocumentAIUsageRecord, RAGDocumentAILimits) (bool, error)
	MarkSentRAGDocumentAIUsage(context.Context, string, IndexFence) (bool, error)
	CommitRAGDocumentAIUsage(context.Context, string, int64, int64, int64, bool) (bool, error)
	ReleaseRAGDocumentAIUsage(context.Context, string) (bool, error)
}

var _ ragFairQueueBudgetLedgerSurface = (*RAGFairQueueStore)(nil)

func TestRAGFairQueueTaskBudgetCreateRequiresLiveFenceEntryPoint(t *testing.T) {
	state := &ragFairQueueStoreDriverState{connectionIDs: []int64{7}}
	st, writer := newRAGFairQueueFacadeTestStore(t, state)
	facade, err := st.BindRAGFairQueueWriter(writer)
	if err != nil {
		t.Fatal(err)
	}
	err = facade.CreateRAGDocumentAITaskBudget(context.Background(),
		&RAGDocumentAITaskBudgetRecord{TaskID: 1, UserID: "u", MaxRequests: 1})
	if !errors.Is(err, ErrRAGDocumentAIInvalidFence) {
		t.Fatalf("unfenced fair task budget error=%v", err)
	}
	state.mu.Lock()
	identityCalls := state.identityCalls
	state.mu.Unlock()
	if identityCalls != 0 {
		t.Fatalf("unfenced fair task budget reached writer %d times", identityCalls)
	}
}

func TestRAGTaskBudgetCreateCoreRequiresExactLiveClaim(t *testing.T) {
	st := openRAGTaskClaimStore(t)
	_, _ = seedRAGTaskDocument(t, st, "doc_budget_create_live_fence", 3)
	ctx := context.Background()
	claim, err := st.ClaimRAGIndexTask(ctx, "budget-create-worker", time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	budget := &RAGDocumentAITaskBudgetRecord{
		TaskID: claim.Fence.TaskID, UserID: claim.Task.UserID,
		MaxRequests: 2, MaxTokens: 20, MaxCostMicroUSD: 200, UpdatedAt: time.Now().UTC(),
	}
	created, err := st.withLiveRAGIndexFenceTx(ctx, claim.Fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			if locked.task.ID != budget.TaskID || locked.task.UserID != budget.UserID {
				return false, ErrRAGDocumentAIInvalidFence
			}
			if err := st.createRAGDocumentAITaskBudgetOn(ctx, tx, budget); err != nil {
				return false, err
			}
			return true, nil
		})
	if err != nil || !created {
		t.Fatalf("create live task budget=%v err=%v", created, err)
	}
	stored, err := st.GetRAGDocumentAITaskBudget(ctx, budget.TaskID)
	if err != nil || stored.UserID != budget.UserID || stored.MaxTokens != budget.MaxTokens {
		t.Fatalf("stored task budget=%+v err=%v", stored, err)
	}

	_, staleTaskID := seedRAGTaskDocument(t, st, "doc_budget_create_stale_fence", 3)
	stale, err := st.ClaimRAGIndexTask(ctx, "budget-create-stale", time.Minute)
	if err != nil || stale == nil || stale.Fence.TaskID != staleTaskID {
		t.Fatalf("stale claim=%+v err=%v", stale, err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE rag_index_tasks
		SET dispatch_generation=claim_generation+1 WHERE id=?`, stale.Fence.TaskID); err != nil {
		t.Fatal(err)
	}
	callbackCalled := false
	changed, err := st.withLiveRAGIndexFenceTx(ctx, stale.Fence,
		func(tx *sql.Tx, locked *ragLockedIndexFence) (bool, error) {
			callbackCalled = true
			return true, st.createRAGDocumentAITaskBudgetOn(ctx, tx,
				&RAGDocumentAITaskBudgetRecord{
					TaskID: stale.Fence.TaskID, UserID: stale.Task.UserID,
					MaxRequests: 1, UpdatedAt: time.Now().UTC(),
				})
		})
	if err != nil || changed || callbackCalled {
		t.Fatalf("stale task budget changed=%v callback=%v err=%v", changed, callbackCalled, err)
	}
	if _, err := st.GetRAGDocumentAITaskBudget(ctx, stale.Fence.TaskID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale task budget mutated ledger: %v", err)
	}
}

func TestRAGDocumentAIBudgetLegacyRejectsExpectedWriterBypassWithoutMutation(t *testing.T) {
	limits := RAGDocumentAILimits{MaxRequests: 3, MaxTokens: 100, MaxCostMicroUSD: 1_000}
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_writer_bypass", limits, limits)
	ctx := context.Background()
	fairFence := fixture.claim.Fence
	fairFence.ExpectedWriterFingerprint = testRAGFairQueueWriterFingerprint
	attempt := ragDocumentAITestUsage(fairFence, "attempt-writer-bypass", "logical-writer-bypass",
		fixture.period, 6, 4, 100)

	reserved, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, fairFence, attempt, limits)
	if reserved || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("unbound fair reserve=%v err=%v", reserved, err)
	}
	if _, err := fixture.store.GetRAGDocumentAIUsage(ctx, attempt.IdempotencyKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbound fair reserve mutated usage: %v", err)
	}
	taskBudget, err := fixture.store.GetRAGDocumentAITaskBudget(ctx, fairFence.TaskID)
	if err != nil || taskBudget.ChargedRequests != 0 || taskBudget.ChargedTokens != 0 ||
		taskBudget.ChargedCostMicroUSD != 0 {
		t.Fatalf("unbound fair reserve mutated task budget=%+v err=%v", taskBudget, err)
	}

	legacyFence := fixture.claim.Fence
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, legacyFence, attempt, limits); err != nil || !ok {
		t.Fatalf("legacy reserve=%v err=%v", ok, err)
	}
	if sent, err := fixture.store.MarkSentRAGDocumentAIUsage(ctx, attempt.IdempotencyKey, fairFence); sent || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("unbound fair MarkSent=%v err=%v", sent, err)
	}
	usage, err := fixture.store.GetRAGDocumentAIUsage(ctx, attempt.IdempotencyKey)
	if err != nil || usage.State != RAGDocumentAIUsageReserved {
		t.Fatalf("unbound fair MarkSent mutated usage=%+v err=%v", usage, err)
	}
}

func TestRAGDocumentAIBudgetLegacyEmptyWriterFenceRegression(t *testing.T) {
	limits := RAGDocumentAILimits{MaxRequests: 2, MaxTokens: 50, MaxCostMicroUSD: 500}
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_legacy_regression", limits, limits)
	attempt := ragDocumentAITestUsage(fixture.claim.Fence, "attempt-legacy-regression",
		"logical-legacy-regression", fixture.period, 3, 2, 50)

	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(context.Background(), fixture.claim.Fence,
		attempt, limits); err != nil || !ok {
		t.Fatalf("legacy reserve=%v err=%v", ok, err)
	}
	if ok, err := fixture.store.ReleaseRAGDocumentAIUsage(context.Background(), attempt.IdempotencyKey); err != nil || !ok {
		t.Fatalf("legacy release=%v err=%v", ok, err)
	}
}

func TestRAGFairQueueBudgetFacadeFailsBeforeLedgerAccessOnWriterMismatch(t *testing.T) {
	state := &ragFairQueueStoreDriverState{connectionIDs: []int64{7}}
	store, actualWriter := newRAGFairQueueFacadeTestStore(t, state)
	wrongWriter := testRAGFairQueueWriterFingerprint
	if wrongWriter == actualWriter {
		t.Fatal("test writer unexpectedly equals discovered writer")
	}
	facade, err := store.BindRAGFairQueueWriter(wrongWriter)
	if err != nil {
		t.Fatal(err)
	}
	fence := IndexFence{
		TaskID: 1, DocID: "doc", DocVersion: 1, ClaimGeneration: 1,
		LeaseOwner: "worker", ExpectedWriterFingerprint: wrongWriter,
	}
	now := time.Now().UTC()
	period := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	usage := ragDocumentAITestUsage(fence, "writer-mismatch-attempt", "writer-mismatch-logical",
		period, 1, 1, 1)

	if ok, err := facade.ReserveRAGDocumentAIUsage(context.Background(), fence, usage,
		RAGDocumentAILimits{MaxRequests: 1, MaxTokens: 2, MaxCostMicroUSD: 1}); ok || !errors.Is(err, ErrFairQueueWriterMismatch) {
		t.Fatalf("writer-mismatch reserve=%v err=%v", ok, err)
	}
	state.mu.Lock()
	identityCalls, physicalClose := state.identityCalls, state.physicalClose
	state.mu.Unlock()
	if identityCalls != 1 || physicalClose != 1 {
		t.Fatalf("identity calls=%d physical closes=%d", identityCalls, physicalClose)
	}
}

func TestRAGFairQueueBudgetReserveFullFenceGuardHasZeroLedgerMutation(t *testing.T) {
	limits := RAGDocumentAILimits{MaxRequests: 3, MaxTokens: 100, MaxCostMicroUSD: 1_000}
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_full_reserve_guard", limits, limits)
	ctx := context.Background()
	fence := fixture.claim.Fence
	fence.ExpectedWriterFingerprint = testRAGFairQueueWriterFingerprint
	if _, err := fixture.store.db.ExecContext(ctx, `UPDATE rag_index_tasks
		SET dispatch_generation=claim_generation+1 WHERE id=?`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	attempt := ragDocumentAITestUsage(fence, "attempt-full-reserve-guard",
		"logical-full-reserve-guard", fixture.period, 6, 4, 100)
	reservedTokens, err := validateRAGDocumentAIReservation(fence, attempt, limits)
	if err != nil {
		t.Fatal(err)
	}
	route, err := fixture.store.ragOwnershipRoute(ctx, fence.DocID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err := fixture.store.reserveRAGDocumentAIUsageTx(ctx, tx, fence, attempt,
		limits, reservedTokens, true, func(ctx context.Context, fence IndexFence) (bool, error) {
			return fixture.store.currentRAGDocumentAIFullFenceTx(ctx, tx, fence, route, attempt.UserID)
		})
	if created || !errors.Is(err, ErrRAGDocumentAIInvalidFence) {
		_ = tx.Rollback()
		t.Fatalf("guarded reserve=%v err=%v", created, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.GetRAGDocumentAIUsage(ctx, attempt.IdempotencyKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("guarded reserve created usage: %v", err)
	}
	taskBudget, err := fixture.store.GetRAGDocumentAITaskBudget(ctx, fence.TaskID)
	if err != nil || taskBudget.ChargedRequests != 0 || taskBudget.ChargedTokens != 0 ||
		taskBudget.ChargedCostMicroUSD != 0 {
		t.Fatalf("guarded reserve charged task budget=%+v err=%v", taskBudget, err)
	}
	if _, err := fixture.store.GetRAGDocumentAIUserBudget(ctx, attempt.UserID, attempt.PeriodStartUTC); !errors.Is(err, ErrNotFound) {
		t.Fatalf("guarded reserve created user aggregate: %v", err)
	}
}

func TestRAGFairQueueBudgetMarkSentFullFenceGuardDoesNotAuthorizeOrRefund(t *testing.T) {
	limits := RAGDocumentAILimits{MaxRequests: 3, MaxTokens: 100, MaxCostMicroUSD: 1_000}
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_full_sent_guard", limits, limits)
	ctx := context.Background()
	fence := fixture.claim.Fence
	attempt := ragDocumentAITestUsage(fence, "attempt-full-sent-guard",
		"logical-full-sent-guard", fixture.period, 6, 4, 100)
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, fence, attempt, limits); err != nil || !ok {
		t.Fatalf("reserve fixture=%v err=%v", ok, err)
	}
	if _, err := fixture.store.db.ExecContext(ctx, `UPDATE rag_index_tasks
		SET dispatch_generation=claim_generation+1 WHERE id=?`, fence.TaskID); err != nil {
		t.Fatal(err)
	}
	fence.ExpectedWriterFingerprint = testRAGFairQueueWriterFingerprint
	preflight, err := fixture.store.GetRAGDocumentAIUsage(ctx, attempt.IdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	route, err := fixture.store.ragOwnershipRoute(ctx, fence.DocID)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := fixture.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	sent, err := fixture.store.markSentRAGDocumentAIUsageTx(ctx, tx, preflight,
		attempt.IdempotencyKey, fence, true,
		func(ctx context.Context, fence IndexFence) (bool, error) {
			return fixture.store.currentRAGDocumentAIFullFenceTx(ctx, tx, fence, route, preflight.UserID)
		})
	if sent || !errors.Is(err, ErrRAGDocumentAIInvalidFence) {
		_ = tx.Rollback()
		t.Fatalf("guarded MarkSent=%v err=%v", sent, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	usage, err := fixture.store.GetRAGDocumentAIUsage(ctx, attempt.IdempotencyKey)
	if err != nil || usage.State != RAGDocumentAIUsageReserved {
		t.Fatalf("guarded MarkSent changed usage=%+v err=%v", usage, err)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 1, 10, 100)
}
