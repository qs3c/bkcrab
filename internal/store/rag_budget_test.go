package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type ragDocumentAIBudgetFixture struct {
	store      *DBStore
	claim      *RAGIndexClaim
	period     time.Time
	userLimits RAGDocumentAILimits
}

func newRAGDocumentAIBudgetFixture(
	t *testing.T,
	docID string,
	taskLimits, userLimits RAGDocumentAILimits,
) *ragDocumentAIBudgetFixture {
	t.Helper()
	st := openRAGTaskClaimStore(t)
	_, taskID := seedRAGTaskDocument(t, st, docID, 3)
	claim, err := st.ClaimRAGIndexTask(context.Background(), "budget-worker-"+docID, 5*time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim budget task: claim=%+v err=%v", claim, err)
	}
	if claim.Fence.TaskID != taskID {
		t.Fatalf("claimed task id = %d, want %d", claim.Fence.TaskID, taskID)
	}
	if err := st.CreateRAGDocumentAITaskBudget(context.Background(), &RAGDocumentAITaskBudgetRecord{
		TaskID: claim.Fence.TaskID, UserID: "u_claim",
		MaxRequests: taskLimits.MaxRequests, MaxTokens: taskLimits.MaxTokens,
		MaxCostMicroUSD: taskLimits.MaxCostMicroUSD,
	}); err != nil {
		t.Fatalf("create task budget: %v", err)
	}
	return &ragDocumentAIBudgetFixture{
		store: st, claim: claim,
		period: func() time.Time {
			now := time.Now().UTC()
			return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		}(),
		userLimits: userLimits,
	}
}

func ragDocumentAITestUsage(
	fence IndexFence,
	key, logicalKey string,
	period time.Time,
	inputTokens, outputTokens, costMicroUSD int64,
) *RAGDocumentAIUsageRecord {
	expiresAt := time.Now().UTC().Add(time.Hour)
	return &RAGDocumentAIUsageRecord{
		IdempotencyKey: key, LogicalRequestKey: logicalKey, UserID: "u_claim",
		DocID: fence.DocID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
		ClaimGeneration: fence.ClaimGeneration, LeaseOwner: fence.LeaseOwner,
		Operation: "vision", ProviderFingerprint: "provider-fingerprint-v1",
		PeriodStartUTC: period, ReservedInputTokens: inputTokens,
		ReservedOutputTokens: outputTokens, EstimatedCostMicroUSD: costMicroUSD,
		ReservationExpiresAt: &expiresAt,
	}
}

func assertRAGDocumentAIBudgetCharges(
	t *testing.T,
	fixture *ragDocumentAIBudgetFixture,
	wantRequests, wantTokens, wantCost int64,
) {
	t.Helper()
	ctx := context.Background()
	taskBudget, err := fixture.store.GetRAGDocumentAITaskBudget(ctx, fixture.claim.Fence.TaskID)
	if err != nil {
		t.Fatalf("get task budget: %v", err)
	}
	userBudget, err := fixture.store.GetRAGDocumentAIUserBudget(ctx, "u_claim", fixture.period)
	if err != nil {
		t.Fatalf("get user budget: %v", err)
	}
	for name, got := range map[string][3]int64{
		"task": {taskBudget.ChargedRequests, taskBudget.ChargedTokens, taskBudget.ChargedCostMicroUSD},
		"user": {userBudget.ChargedRequests, userBudget.ChargedTokens, userBudget.ChargedCostMicroUSD},
	} {
		if got != [3]int64{wantRequests, wantTokens, wantCost} {
			t.Errorf("%s charges = %v, want [%d %d %d]", name, got, wantRequests, wantTokens, wantCost)
		}
	}
}

func reclaimRAGDocumentAIBudgetTask(t *testing.T, fixture *ragDocumentAIBudgetFixture) *RAGIndexClaim {
	t.Helper()
	ctx := context.Background()
	if _, err := fixture.store.db.ExecContext(ctx,
		`UPDATE rag_index_tasks SET lease_until='2000-01-01 00:00:00' WHERE id=?`,
		fixture.claim.Fence.TaskID); err != nil {
		t.Fatalf("expire budget task lease: %v", err)
	}
	claim, err := fixture.store.ClaimRAGIndexTask(ctx, "budget-reclaimer-"+fixture.claim.Fence.DocID, 5*time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("reclaim budget task: claim=%+v err=%v", claim, err)
	}
	return claim
}

func TestRAGDocumentAIBudgetConcurrentLastQuotaAndIdempotency(t *testing.T) {
	limits := RAGDocumentAILimits{MaxRequests: 1, MaxTokens: 50, MaxCostMicroUSD: 500}
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_concurrent", limits, limits)
	ctx := context.Background()
	requests := []*RAGDocumentAIUsageRecord{
		ragDocumentAITestUsage(fixture.claim.Fence, "attempt-concurrent-a", "logical-a", fixture.period, 40, 10, 500),
		ragDocumentAITestUsage(fixture.claim.Fence, "attempt-concurrent-b", "logical-b", fixture.period, 40, 10, 500),
	}
	type result struct {
		index    int
		reserved bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, len(requests))
	var wg sync.WaitGroup
	for index, request := range requests {
		index, request := index, request
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			reserved, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, fixture.claim.Fence, request, limits)
			results <- result{index: index, reserved: reserved, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	winner := -1
	budgetFailures := 0
	for item := range results {
		switch {
		case item.reserved && item.err == nil:
			if winner != -1 {
				t.Fatalf("multiple reservations won: %d and %d", winner, item.index)
			}
			winner = item.index
		case !item.reserved && errors.Is(item.err, ErrRAGDocumentAIBudgetExceeded):
			budgetFailures++
		default:
			t.Fatalf("unexpected reserve result %+v", item)
		}
	}
	if winner == -1 || budgetFailures != 1 {
		t.Fatalf("winner=%d budget failures=%d", winner, budgetFailures)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 1, 50, 500)

	reserved, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, fixture.claim.Fence, requests[winner], limits)
	if err != nil || reserved {
		t.Fatalf("exact duplicate reserve = %v, %v; want false,nil", reserved, err)
	}
	conflict := *requests[winner]
	conflict.ReservedInputTokens++
	if reserved, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, fixture.claim.Fence, &conflict, limits); reserved || !errors.Is(err, ErrRAGDocumentAIUsageConflict) {
		t.Fatalf("conflicting duplicate = %v, %v", reserved, err)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 1, 50, 500)

	if released, err := fixture.store.ReleaseRAGDocumentAIUsage(ctx, requests[winner].IdempotencyKey); err != nil || !released {
		t.Fatalf("release winner = %v, %v", released, err)
	}
	if released, err := fixture.store.ReleaseRAGDocumentAIUsage(ctx, requests[winner].IdempotencyKey); err != nil || released {
		t.Fatalf("idempotent release = %v, %v", released, err)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 0, 0, 0)
}

func TestRAGDocumentAIBudgetStaleFenceCannotReserveOrSend(t *testing.T) {
	limits := RAGDocumentAILimits{MaxRequests: 3, MaxTokens: 100, MaxCostMicroUSD: 1_000}
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_stale", limits, limits)
	ctx := context.Background()
	oldFence := fixture.claim.Fence
	reservedAttempt := ragDocumentAITestUsage(oldFence, "attempt-stale-reserved", "logical-stale", fixture.period, 6, 4, 100)
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, oldFence, reservedAttempt, limits); err != nil || !ok {
		t.Fatalf("reserve old attempt = %v, %v", ok, err)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 1, 10, 100)

	current := reclaimRAGDocumentAIBudgetTask(t, fixture)
	staleAttempt := ragDocumentAITestUsage(oldFence, "attempt-after-reclaim", "logical-after-reclaim", fixture.period, 1, 1, 10)
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, oldFence, staleAttempt, limits); ok || !errors.Is(err, ErrRAGDocumentAIInvalidFence) {
		t.Fatalf("stale reserve = %v, %v", ok, err)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 1, 10, 100)

	if send, err := fixture.store.MarkSentRAGDocumentAIUsage(ctx, reservedAttempt.IdempotencyKey, oldFence); err != nil || send {
		t.Fatalf("stale MarkSent = %v, %v; want released without send", send, err)
	}
	usage, err := fixture.store.GetRAGDocumentAIUsage(ctx, reservedAttempt.IdempotencyKey)
	if err != nil || usage.State != RAGDocumentAIUsageReleased {
		t.Fatalf("stale reservation = %+v, %v", usage, err)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 0, 0, 0)

	currentAttempt := ragDocumentAITestUsage(current.Fence, "attempt-current", "logical-current", fixture.period, 6, 4, 100)
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, current.Fence, currentAttempt, limits); err != nil || !ok {
		t.Fatalf("reserve current attempt = %v, %v", ok, err)
	}
	if send, err := fixture.store.MarkSentRAGDocumentAIUsage(ctx, currentAttempt.IdempotencyKey, oldFence); send || !errors.Is(err, ErrRAGDocumentAIInvalidFence) {
		t.Fatalf("wrong fence MarkSent = %v, %v", send, err)
	}
	usage, err = fixture.store.GetRAGDocumentAIUsage(ctx, currentAttempt.IdempotencyKey)
	if err != nil || usage.State != RAGDocumentAIUsageReserved {
		t.Fatalf("wrong fence released another attempt: %+v, %v", usage, err)
	}
}

func TestRAGDocumentAIBudgetSentLateCommitOverrunAndNoRelease(t *testing.T) {
	taskLimits := RAGDocumentAILimits{MaxRequests: 3, MaxTokens: 20, MaxCostMicroUSD: 200}
	userLimits := taskLimits
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_late", taskLimits, userLimits)
	ctx := context.Background()
	attempt := ragDocumentAITestUsage(fixture.claim.Fence, "attempt-late", "logical-late", fixture.period, 5, 5, 100)
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, fixture.claim.Fence, attempt, userLimits); err != nil || !ok {
		t.Fatalf("reserve = %v, %v", ok, err)
	}
	if send, err := fixture.store.MarkSentRAGDocumentAIUsage(ctx, attempt.IdempotencyKey, fixture.claim.Fence); err != nil || !send {
		t.Fatalf("MarkSent = %v, %v", send, err)
	}
	if send, err := fixture.store.MarkSentRAGDocumentAIUsage(ctx, attempt.IdempotencyKey, fixture.claim.Fence); err != nil || send {
		t.Fatalf("duplicate MarkSent = %v, %v", send, err)
	}
	if released, err := fixture.store.ReleaseRAGDocumentAIUsage(ctx, attempt.IdempotencyKey); err != nil || released {
		t.Fatalf("release SENT = %v, %v", released, err)
	}

	current := reclaimRAGDocumentAIBudgetTask(t, fixture)
	if committed, err := fixture.store.CommitRAGDocumentAIUsage(ctx, attempt.IdempotencyKey, 8, 2, 150, false); err != nil || !committed {
		t.Fatalf("late overrun commit = %v, %v", committed, err)
	}
	if committed, err := fixture.store.CommitRAGDocumentAIUsage(ctx, attempt.IdempotencyKey, 8, 2, 150, false); err != nil || committed {
		t.Fatalf("idempotent commit = %v, %v", committed, err)
	}
	usage, err := fixture.store.GetRAGDocumentAIUsage(ctx, attempt.IdempotencyKey)
	if err != nil || usage.State != RAGDocumentAIUsageOverrun ||
		usage.ActualInputTokens != 8 || usage.ActualOutputTokens != 2 ||
		usage.EstimatedCostMicroUSD != 150 {
		t.Fatalf("overrun usage = %+v, %v", usage, err)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 1, 10, 150)

	blocked := ragDocumentAITestUsage(current.Fence, "attempt-blocked-after-overrun", "logical-new", fixture.period, 0, 0, 0)
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, current.Fence, blocked, userLimits); ok || !errors.Is(err, ErrRAGDocumentAIBudgetExceeded) {
		t.Fatalf("reserve after overrun = %v, %v", ok, err)
	}
}

func TestRAGDocumentAIBudgetReconcilesExpiredReservedAndAbandonedSent(t *testing.T) {
	limits := RAGDocumentAILimits{MaxRequests: 4, MaxTokens: 100, MaxCostMicroUSD: 1_000}
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_reconcile", limits, limits)
	ctx := context.Background()

	expired := ragDocumentAITestUsage(fixture.claim.Fence, "attempt-expired-reserved", "logical-expired", fixture.period, 7, 3, 100)
	past := time.Now().UTC().Add(-time.Hour)
	expired.ReservationExpiresAt = &past
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, fixture.claim.Fence, expired, limits); err != nil || !ok {
		t.Fatalf("reserve expired fixture = %v, %v", ok, err)
	}
	future := time.Now().UTC().Add(time.Hour)
	pastCutoff := time.Now().UTC().Add(-24 * time.Hour)
	if count, err := fixture.store.ReconcileRAGDocumentAIUsage(ctx, future, pastCutoff, 10); err != nil || count != 0 {
		t.Fatalf("valid-fence RESERVED reconcile = %d, %v; want 0,nil", count, err)
	}
	stillReserved, err := fixture.store.GetRAGDocumentAIUsage(ctx, expired.IdempotencyKey)
	if err != nil || stillReserved.State != RAGDocumentAIUsageReserved {
		t.Fatalf("valid-fence reservation was released: %+v, %v", stillReserved, err)
	}
	abandoned := ragDocumentAITestUsage(fixture.claim.Fence, "attempt-abandoned-sent", "logical-abandoned", fixture.period, 8, 2, 120)
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, fixture.claim.Fence, abandoned, limits); err != nil || !ok {
		t.Fatalf("reserve sent fixture = %v, %v", ok, err)
	}
	if send, err := fixture.store.MarkSentRAGDocumentAIUsage(ctx, abandoned.IdempotencyKey, fixture.claim.Fence); err != nil || !send {
		t.Fatalf("mark abandoned sent = %v, %v", send, err)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 2, 20, 220)

	current := reclaimRAGDocumentAIBudgetTask(t, fixture)
	count, err := fixture.store.ReconcileRAGDocumentAIUsage(ctx, future, future, 10)
	if err != nil || count != 2 {
		t.Fatalf("reconcile = %d, %v; want 2,nil", count, err)
	}
	expiredUsage, err := fixture.store.GetRAGDocumentAIUsage(ctx, expired.IdempotencyKey)
	if err != nil || expiredUsage.State != RAGDocumentAIUsageReleased {
		t.Fatalf("expired usage = %+v, %v", expiredUsage, err)
	}
	sentUsage, err := fixture.store.GetRAGDocumentAIUsage(ctx, abandoned.IdempotencyKey)
	if err != nil || sentUsage.State != RAGDocumentAIUsageCommitted || !sentUsage.UsageEstimated ||
		sentUsage.ActualInputTokens != abandoned.ReservedInputTokens ||
		sentUsage.ActualOutputTokens != abandoned.ReservedOutputTokens {
		t.Fatalf("abandoned sent usage = %+v, %v", sentUsage, err)
	}
	assertRAGDocumentAIBudgetCharges(t, fixture, 1, 10, 120)
	retryWithoutCache := ragDocumentAITestUsage(current.Fence, "attempt-logical-cache-miss", abandoned.LogicalRequestKey, fixture.period, 1, 1, 10)
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(ctx, current.Fence, retryWithoutCache, limits); err != nil || !ok {
		t.Fatalf("settled usage without a durable cache suppressed retry = %v, %v", ok, err)
	}
	if count, err := fixture.store.ReconcileRAGDocumentAIUsage(ctx, future, future, 10); err != nil || count != 0 {
		t.Fatalf("idempotent reconcile = %d, %v", count, err)
	}
}

func TestRAGDocumentAITaskBudgetCreateRejectsConflictingSnapshot(t *testing.T) {
	limits := RAGDocumentAILimits{MaxRequests: 4, MaxTokens: 100, MaxCostMicroUSD: 1_000}
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_snapshot_conflict", limits, limits)
	ctx := context.Background()
	if err := fixture.store.CreateRAGDocumentAITaskBudget(ctx, nil); err == nil {
		t.Fatal("nil task budget must be rejected")
	}
	existing, err := fixture.store.GetRAGDocumentAITaskBudget(ctx, fixture.claim.Fence.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.CreateRAGDocumentAITaskBudget(ctx, &RAGDocumentAITaskBudgetRecord{
		TaskID: existing.TaskID, UserID: existing.UserID,
		MaxRequests: existing.MaxRequests, MaxTokens: existing.MaxTokens,
		MaxCostMicroUSD: existing.MaxCostMicroUSD,
	}); err != nil {
		t.Fatalf("idempotent task budget create: %v", err)
	}
	if err := fixture.store.CreateRAGDocumentAITaskBudget(ctx, &RAGDocumentAITaskBudgetRecord{
		TaskID: existing.TaskID, UserID: "different_user",
		MaxRequests: existing.MaxRequests + 1, MaxTokens: existing.MaxTokens,
		MaxCostMicroUSD: existing.MaxCostMicroUSD,
	}); !errors.Is(err, ErrRAGDocumentAIUsageConflict) {
		t.Fatalf("conflicting task budget error = %v", err)
	}
}

func TestRAGDocumentAIBudgetRejectsNonCurrentUTCPeriod(t *testing.T) {
	limits := RAGDocumentAILimits{MaxRequests: 4, MaxTokens: 100, MaxCostMicroUSD: 1_000}
	fixture := newRAGDocumentAIBudgetFixture(t, "doc_budget_period", limits, limits)
	wrongPeriod := time.Now().UTC().AddDate(0, 0, -1)
	attempt := ragDocumentAITestUsage(fixture.claim.Fence, "attempt-wrong-period", "logical-wrong-period", wrongPeriod, 1, 1, 0)
	if ok, err := fixture.store.ReserveRAGDocumentAIUsage(context.Background(), fixture.claim.Fence, attempt, limits); ok || err == nil {
		t.Fatalf("wrong UTC period reserve = %v, %v; want rejection", ok, err)
	}
	if _, err := fixture.store.GetRAGDocumentAIUserBudget(context.Background(), "u_claim", wrongPeriod); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-period aggregate should not be created, err=%v", err)
	}
}
