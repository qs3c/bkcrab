package vision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	OperationPage        = "vision_page"
	OperationPageRepair  = "vision_page_repair"
	OperationImage       = "vision_image"
	OperationImageRepair = "vision_image_repair"
	OperationEnrichment  = "enrichment"
)

type BudgetLedger interface {
	CreateRAGDocumentAITaskBudget(context.Context, *store.RAGDocumentAITaskBudgetRecord) error
	GetRAGDocumentAIUsage(context.Context, string) (*store.RAGDocumentAIUsageRecord, error)
	ReserveRAGDocumentAIUsage(context.Context, store.IndexFence, *store.RAGDocumentAIUsageRecord, store.RAGDocumentAILimits) (bool, error)
	MarkSentRAGDocumentAIUsage(context.Context, string, store.IndexFence) (bool, error)
	CommitRAGDocumentAIUsage(context.Context, string, int64, int64, int64, bool) (bool, error)
	ReleaseRAGDocumentAIUsage(context.Context, string) (bool, error)
}

type TaskBudgetConfig struct {
	Fence          store.IndexFence
	UserID         string
	TaskLimits     store.RAGDocumentAILimits
	UserLimits     store.RAGDocumentAILimits
	ReservationTTL time.Duration
	Now            func() time.Time
	Recorder       telemetry.Recorder
}

// TaskDocumentAIBudget is a thin, concurrency-safe façade over the durable
// SQL state machine. It owns no spend counters; process crashes and lease
// reclaim therefore cannot reset either task or user-period usage.
type TaskDocumentAIBudget struct {
	ledger   BudgetLedger
	config   TaskBudgetConfig
	recorder telemetry.Recorder

	ensureMu sync.Mutex
	ensured  bool
}

func NewTaskDocumentAIBudget(ledger BudgetLedger, config TaskBudgetConfig) *TaskDocumentAIBudget {
	if config.ReservationTTL <= 0 {
		config.ReservationTTL = 5 * time.Minute
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Recorder == nil {
		config.Recorder = telemetry.NewSlogRecorder(nil)
	}
	return &TaskDocumentAIBudget{ledger: ledger, config: config, recorder: config.Recorder}
}

func (b *TaskDocumentAIBudget) Fence() store.IndexFence {
	if b == nil {
		return store.IndexFence{}
	}
	return b.config.Fence
}

type AttemptRequest struct {
	LogicalRequestKey     string
	Operation             string
	ProviderFingerprint   string
	Attempt               int
	InputTokens           int64
	OutputTokens          int64
	EstimatedCostMicroUSD int64
}

type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CostMicroUSD int64
	Estimated    bool
}

func (b *TaskDocumentAIBudget) Reserve(ctx context.Context, fence store.IndexFence, request AttemptRequest) (*Reservation, error) {
	if b == nil || b.ledger == nil {
		return nil, ErrBudgetRequired
	}
	if err := b.validate(fence, request); err != nil {
		b.recordBudget(ctx, fence, request, "reserve", "rejected", budgetErrorCode(err))
		return nil, err
	}
	if err := b.ensureTaskBudget(ctx); err != nil {
		b.recordBudget(ctx, fence, request, "reserve", "error", "task_budget_init")
		return nil, err
	}
	now := b.config.Now().UTC()
	period := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	logicalKey := canonicalLedgerKey(request.LogicalRequestKey)
	providerFingerprint := canonicalLedgerKey(request.ProviderFingerprint)
	key := AttemptKey(fence, logicalKey, request.Operation, request.Attempt)
	expires := now.Add(b.config.ReservationTTL)
	usage := &store.RAGDocumentAIUsageRecord{
		IdempotencyKey: key, LogicalRequestKey: logicalKey, UserID: b.config.UserID,
		DocID: fence.DocID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
		ClaimGeneration: fence.ClaimGeneration, LeaseOwner: fence.LeaseOwner,
		Operation: request.Operation, ProviderFingerprint: providerFingerprint,
		PeriodStartUTC: period, ReservedInputTokens: request.InputTokens,
		ReservedOutputTokens:  request.OutputTokens,
		EstimatedCostMicroUSD: request.EstimatedCostMicroUSD,
		ReservationExpiresAt:  &expires,
	}
	created, err := b.ledger.ReserveRAGDocumentAIUsage(ctx, fence, usage, b.config.UserLimits)
	if err != nil {
		if errors.Is(err, store.ErrRAGDocumentAIBudgetExceeded) {
			b.recordBudget(ctx, fence, request, "reserve", "rejected", "quota_exceeded")
			return nil, &Error{Kind: ErrorBudget, Err: err}
		}
		b.recordBudget(ctx, fence, request, "reserve", "error", budgetErrorCode(err))
		return nil, err
	}
	if !created {
		existing, lookupErr := b.ledger.GetRAGDocumentAIUsage(ctx, key)
		if lookupErr != nil {
			if errors.Is(lookupErr, store.ErrNotFound) {
				b.recordBudget(ctx, fence, request, "reserve", "rejected", "committed_cache")
				return nil, ErrCacheCommitted
			}
			b.recordBudget(ctx, fence, request, "reserve", "error", "usage_lookup")
			return nil, lookupErr
		}
		if existing.State == store.RAGDocumentAIUsageCommitted || existing.State == store.RAGDocumentAIUsageOverrun {
			b.recordBudget(ctx, fence, request, "reserve", "rejected", "committed_cache")
			return nil, ErrCacheCommitted
		}
		if existing.State != store.RAGDocumentAIUsageReserved {
			b.recordBudget(ctx, fence, request, "reserve", "rejected", "invalid_usage_state")
			return nil, ErrAttemptNotSent
		}
	}
	b.recordBudget(ctx, fence, request, "reserve", "ok", "")
	return &Reservation{
		ledger: b.ledger, key: key, fence: fence,
		reservedInput: request.InputTokens, reservedOutput: request.OutputTokens,
		reservedCost: request.EstimatedCostMicroUSD, state: reservationReserved,
		recorder: b.recorder, operation: request.Operation, attempt: request.Attempt,
	}, nil
}

func (b *TaskDocumentAIBudget) recordBudget(
	ctx context.Context,
	fence store.IndexFence,
	request AttemptRequest,
	transition, outcome, errorCode string,
) {
	if b == nil {
		return
	}
	telemetry.Emit(ctx, b.recorder, telemetry.EventDocumentAIBudget, telemetry.Fields{
		DocID: fence.DocID, TaskID: fence.TaskID, DocVersion: fence.DocVersion,
		ClaimGeneration: fence.ClaimGeneration, Operation: request.Operation,
		Transition: transition, Outcome: outcome, ErrorCode: errorCode, Attempt: request.Attempt,
		InputTokens: request.InputTokens, OutputTokens: request.OutputTokens,
		CostMicroUSD: request.EstimatedCostMicroUSD,
	})
}

func budgetErrorCode(err error) string {
	switch {
	case errors.Is(err, store.ErrRAGDocumentAIBudgetExceeded):
		return "quota_exceeded"
	case errors.Is(err, store.ErrRAGDocumentAIInvalidFence):
		return "invalid_fence"
	case errors.Is(err, store.ErrRAGDocumentAIUsageConflict):
		return "usage_conflict"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "ledger_error"
	}
}

func (b *TaskDocumentAIBudget) validate(fence store.IndexFence, request AttemptRequest) error {
	if b.config.UserID == "" || strings.TrimSpace(b.config.UserID) != b.config.UserID {
		return errors.New("vision: budget user ID is required")
	}
	if fence != b.config.Fence || fence.TaskID <= 0 || fence.DocID == "" || fence.DocVersion <= 0 ||
		fence.ClaimGeneration <= 0 || fence.LeaseOwner == "" {
		return store.ErrRAGDocumentAIInvalidFence
	}
	if request.LogicalRequestKey == "" || request.ProviderFingerprint == "" || request.Attempt < 0 ||
		request.InputTokens < 0 || request.OutputTokens < 0 || request.EstimatedCostMicroUSD < 0 {
		return errors.New("vision: invalid DocumentAI attempt reservation")
	}
	switch request.Operation {
	case OperationPage, OperationPageRepair, OperationImage, OperationImageRepair, OperationEnrichment:
	default:
		return fmt.Errorf("vision: unsupported DocumentAI operation %q", request.Operation)
	}
	return nil
}

func (b *TaskDocumentAIBudget) ensureTaskBudget(ctx context.Context) error {
	b.ensureMu.Lock()
	defer b.ensureMu.Unlock()
	if b.ensured {
		return nil
	}
	limits := b.config.TaskLimits
	if limits.MaxRequests < 0 || limits.MaxTokens < 0 || limits.MaxCostMicroUSD < 0 {
		return errors.New("vision: invalid task DocumentAI limits")
	}
	err := b.ledger.CreateRAGDocumentAITaskBudget(ctx, &store.RAGDocumentAITaskBudgetRecord{
		TaskID: b.config.Fence.TaskID, UserID: b.config.UserID,
		MaxRequests: limits.MaxRequests, MaxTokens: limits.MaxTokens,
		MaxCostMicroUSD: limits.MaxCostMicroUSD,
	})
	if err == nil {
		b.ensured = true
	}
	return err
}

func AttemptKey(fence store.IndexFence, logicalRequestKey, operation string, attempt int) string {
	return framedSHA256(
		[]byte("document-ai-attempt-v1"), []byte(strconv.FormatInt(fence.TaskID, 10)),
		[]byte(fence.DocID), []byte(strconv.FormatInt(fence.DocVersion, 10)),
		[]byte(strconv.FormatInt(fence.ClaimGeneration, 10)), []byte(fence.LeaseOwner),
		[]byte(logicalRequestKey), []byte(operation), []byte(strconv.Itoa(attempt)),
	)
}

func LogicalRequestKey(parts ...string) string {
	values := make([][]byte, 0, len(parts)+1)
	values = append(values, []byte("document-ai-logical-v1"))
	for _, part := range parts {
		values = append(values, []byte(part))
	}
	return framedSHA256(values...)
}

func canonicalLedgerKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if document.CanonicalSHA256(value) {
		return value
	}
	return framedSHA256([]byte(value))
}

func framedSHA256(parts ...[]byte) string {
	h := sha256.New()
	var frame [8]byte
	for _, part := range parts {
		length := uint64(len(part))
		for i := 7; i >= 0; i-- {
			frame[i] = byte(length)
			length >>= 8
		}
		_, _ = h.Write(frame[:])
		_, _ = h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type reservationState uint8

const (
	reservationReserved reservationState = iota + 1
	reservationSent
	reservationDone
)

type Reservation struct {
	ledger                                      BudgetLedger
	key                                         string
	fence                                       store.IndexFence
	reservedInput, reservedOutput, reservedCost int64
	recorder                                    telemetry.Recorder
	operation                                   string
	attempt                                     int
	mu                                          sync.Mutex
	state                                       reservationState
}

func (r *Reservation) Key() string {
	if r == nil {
		return ""
	}
	return r.key
}

func (r *Reservation) MarkSent(ctx context.Context, fence store.IndexFence) error {
	if r == nil {
		return ErrAttemptNotSent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if fence != r.fence {
		r.record(ctx, "sent", "rejected", "invalid_fence", Usage{})
		return store.ErrRAGDocumentAIInvalidFence
	}
	if r.state == reservationSent {
		return nil
	}
	if r.state != reservationReserved {
		r.record(ctx, "sent", "rejected", "invalid_usage_state", Usage{})
		return ErrAttemptNotSent
	}
	ok, err := r.ledger.MarkSentRAGDocumentAIUsage(ctx, r.key, fence)
	if err != nil {
		r.record(ctx, "sent", "error", budgetErrorCode(err), Usage{})
		return err
	}
	if !ok {
		r.record(ctx, "sent", "rejected", "invalid_usage_state", Usage{})
		return ErrAttemptNotSent
	}
	r.state = reservationSent
	r.record(ctx, "sent", "ok", "", Usage{})
	return nil
}

func (r *Reservation) Release(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == reservationDone {
		return nil
	}
	if r.state != reservationReserved {
		r.record(ctx, "release", "rejected", "invalid_usage_state", Usage{})
		return errors.New("vision: SENT DocumentAI usage cannot be released")
	}
	_, err := r.ledger.ReleaseRAGDocumentAIUsage(ctx, r.key)
	if err == nil {
		r.state = reservationDone
		r.record(ctx, "release", "ok", "", Usage{})
	} else {
		r.record(ctx, "release", "error", budgetErrorCode(err), Usage{})
	}
	return err
}

func (r *Reservation) Commit(ctx context.Context, usage Usage) error {
	if r == nil {
		return ErrAttemptNotSent
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state == reservationDone {
		return nil
	}
	if r.state != reservationSent {
		r.record(ctx, "commit", "rejected", "invalid_usage_state", usage)
		return ErrAttemptNotSent
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CostMicroUSD < 0 {
		r.record(ctx, "commit", "rejected", "invalid_usage", Usage{})
		return errors.New("vision: invalid DocumentAI actual usage")
	}
	_, err := r.ledger.CommitRAGDocumentAIUsage(ctx, r.key, usage.InputTokens,
		usage.OutputTokens, usage.CostMicroUSD, usage.Estimated)
	if err == nil {
		r.state = reservationDone
		transition := "commit"
		if usage.InputTokens+usage.OutputTokens > r.reservedInput+r.reservedOutput || usage.CostMicroUSD > r.reservedCost {
			transition = "commit_overrun"
		}
		r.record(ctx, transition, "ok", "", usage)
	} else {
		r.record(ctx, "commit", "error", budgetErrorCode(err), usage)
	}
	return err
}

func (r *Reservation) CommitEstimated(ctx context.Context) error {
	return r.Commit(ctx, Usage{InputTokens: r.reservedInput, OutputTokens: r.reservedOutput,
		CostMicroUSD: r.reservedCost, Estimated: true})
}

func (r *Reservation) record(ctx context.Context, transition, outcome, errorCode string, usage Usage) {
	if r == nil {
		return
	}
	telemetry.Emit(ctx, r.recorder, telemetry.EventDocumentAIBudget, telemetry.Fields{
		DocID: r.fence.DocID, TaskID: r.fence.TaskID, DocVersion: r.fence.DocVersion,
		ClaimGeneration: r.fence.ClaimGeneration, Operation: r.operation,
		Transition: transition, Outcome: outcome, ErrorCode: errorCode, Attempt: r.attempt,
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens,
		CostMicroUSD: usage.CostMicroUSD, Estimated: usage.Estimated,
	})
}
