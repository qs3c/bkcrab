package fairqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

const (
	defaultRabbitRepairLockTTL     = 30 * time.Second
	defaultRabbitRepairLockRenewal = 10 * time.Second
)

// RabbitRepairRecovery is the common rebuild half used after the broker-loss
// CAS pass. RecoveryCoordinator implements this interface; keeping the seam
// small makes the special-operation ordering independently testable.
type RabbitRepairRecovery interface {
	DrainAttempts(ctx context.Context, config ResourceConfig) error
	RunRecovery(ctx context.Context, config ResourceConfig, fence RecoveryFence, source RecoverySource) error
	FinishRecovery(ctx context.Context, config ResourceConfig, fence RecoveryFence) (ResourceFence, error)
}

type RabbitDisasterRepairOptions struct {
	RecoveryLockTTL   time.Duration
	LockRenewInterval time.Duration
}

func (o RabbitDisasterRepairOptions) withDefaults() (RabbitDisasterRepairOptions, error) {
	if o.RecoveryLockTTL == 0 {
		o.RecoveryLockTTL = defaultRabbitRepairLockTTL
	}
	if o.LockRenewInterval == 0 {
		o.LockRenewInterval = defaultRabbitRepairLockRenewal
	}
	if o.RecoveryLockTTL <= 0 || o.RecoveryLockTTL > maxResourceDuration ||
		o.LockRenewInterval <= 0 || o.LockRenewInterval >= o.RecoveryLockTTL {
		return RabbitDisasterRepairOptions{}, fmt.Errorf("%w: invalid Rabbit repair lock timing", ErrInvalidModel)
	}
	return o, nil
}

type rabbitRepairTokenSource interface {
	Next() (string, error)
}

type cryptoRabbitRepairTokenSource struct{}

func (cryptoRabbitRepairTokenSource) Next() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

// RabbitDisasterRepair owns the explicitly-attested Rabbit data-loss workflow.
// It never publishes while Redis is RECOVERING: RearmAfterBrokerLoss creates
// durable marker=NULL obligations, and the ordinary READY-fenced Dispatcher
// performs mandatory confirmed publication after FinishRecovery.
type RabbitDisasterRepair struct {
	config         ResourceConfig
	writer         WriterIdentity
	broker         BrokerRepairSource
	recoverySource RecoverySource
	journal        OperationJournal
	coordinator    Coordinator
	recovery       RabbitRepairRecovery
	options        RabbitDisasterRepairOptions
	tokens         rabbitRepairTokenSource
}

func NewRabbitDisasterRepair(
	config ResourceConfig,
	writer WriterIdentity,
	broker BrokerRepairSource,
	recoverySource RecoverySource,
	journal OperationJournal,
	coordinator Coordinator,
	recovery RabbitRepairRecovery,
	options RabbitDisasterRepairOptions,
) (*RabbitDisasterRepair, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("fairqueue: Rabbit repair config: %w", err)
	}
	if err := writer.Validate(); err != nil {
		return nil, fmt.Errorf("fairqueue: Rabbit repair writer: %w", err)
	}
	if broker == nil || recoverySource == nil || journal == nil || coordinator == nil || recovery == nil {
		return nil, errors.New("fairqueue: Rabbit repair dependencies are required")
	}
	normalized, err := options.withDefaults()
	if err != nil {
		return nil, err
	}
	return &RabbitDisasterRepair{
		config: config, writer: writer, broker: broker, recoverySource: recoverySource,
		journal: journal, coordinator: coordinator, recovery: recovery,
		options: normalized, tokens: cryptoRabbitRepairTokenSource{},
	}, nil
}

func (r *RabbitDisasterRepair) Check(ctx context.Context) (RabbitRepairReport, error) {
	if r == nil {
		return RabbitRepairReport{}, errors.New("fairqueue: nil Rabbit disaster repair")
	}
	if ctx == nil {
		return RabbitRepairReport{}, errors.New("fairqueue: nil Rabbit repair context")
	}
	if err := ctx.Err(); err != nil {
		return RabbitRepairReport{}, err
	}
	record, found, err := r.journal.Read(ctx, r.config.Key, r.writer.Fingerprint)
	if err != nil {
		return RabbitRepairReport{}, fmt.Errorf("fairqueue: read Rabbit repair journal: %w", err)
	}
	operation, err := rabbitRepairOperationSummary(record, found, r.config.Key, r.writer.Fingerprint)
	if err != nil {
		return RabbitRepairReport{}, err
	}
	highWater, err := r.broker.CaptureRepairHighWater(ctx)
	if err != nil {
		return RabbitRepairReport{}, fmt.Errorf("fairqueue: capture Rabbit repair high water: %w", err)
	}
	if err := ValidateHighWater(highWater); err != nil {
		return RabbitRepairReport{}, fmt.Errorf("%w: invalid Rabbit repair high water: %v", ErrInvalidRecoveryState, err)
	}
	candidates, pages, err := r.scanBrokerCandidates(ctx, highWater, nil, nil, nil)
	if err != nil {
		return RabbitRepairReport{}, err
	}
	report := RabbitRepairReport{
		Resource: r.config.Key, Writer: r.writer, CandidateCount: candidates,
		PagesScanned: pages, Operation: operation,
	}
	if err := report.Validate(); err != nil {
		return RabbitRepairReport{}, err
	}
	return report, nil
}

func rabbitRepairOperationSummary(
	record RecoveryOperationRecord,
	found bool,
	resource, writer string,
) (OperationSummary, error) {
	if !found {
		return OperationSummary{}, nil
	}
	if err := record.ValidatePersisted(); err != nil || record.Resource != resource {
		return OperationSummary{}, fmt.Errorf("%w: Rabbit repair journal identity mismatch", ErrInvalidOperationRecord)
	}
	if record.CurrentWriterFingerprint != writer {
		return OperationSummary{}, ErrAuthoritativeWriterMismatch
	}
	return OperationSummary{Present: true, Kind: record.Kind, Phase: record.Phase}, nil
}

func (r *RabbitDisasterRepair) scanBrokerCandidates(
	ctx context.Context,
	highWater string,
	beforePage func(context.Context) error,
	visit func(context.Context, DispatchCandidate) error,
	afterPage func(context.Context) error,
) (int64, int64, error) {
	after := ""
	var candidateCount int64
	var pageCount int64
	for {
		if err := ctx.Err(); err != nil {
			return candidateCount, pageCount, err
		}
		if beforePage != nil {
			if err := beforePage(ctx); err != nil {
				return candidateCount, pageCount, err
			}
		}
		page, err := r.broker.ListBrokerBackedCandidates(ctx, highWater, after, r.config.ReconcilePageSize)
		if err != nil {
			return candidateCount, pageCount, fmt.Errorf("fairqueue: list Rabbit repair candidates: %w", err)
		}
		if err := page.Validate(after, r.config.ReconcilePageSize); err != nil {
			return candidateCount, pageCount, err
		}
		if int64(len(page.Items)) > math.MaxInt64-candidateCount || pageCount == math.MaxInt64 {
			return candidateCount, pageCount, fmt.Errorf("%w: Rabbit repair aggregate overflow", ErrInvalidRecoveryState)
		}
		pageCount++
		candidateCount += int64(len(page.Items))
		for _, candidate := range page.Items {
			if err := r.validateCandidate(candidate); err != nil {
				return candidateCount, pageCount, err
			}
			if visit != nil {
				if err := visit(ctx, candidate); err != nil {
					return candidateCount, pageCount, err
				}
			}
		}
		if afterPage != nil {
			if err := afterPage(ctx); err != nil {
				return candidateCount, pageCount, err
			}
		}
		if page.Done {
			return candidateCount, pageCount, nil
		}
		after = page.NextCursor
	}
}

func (r *RabbitDisasterRepair) validateCandidate(candidate DispatchCandidate) error {
	if err := candidate.Validate(); err != nil || candidate.Message.Resource != r.config.Key ||
		!r.config.ValidateTaskID(candidate.Message.TaskID) {
		return fmt.Errorf("%w: invalid Rabbit repair candidate", ErrInvalidRecoveryPage)
	}
	return nil
}

func (r *RabbitDisasterRepair) Apply(ctx context.Context, attestation RabbitRepairAttestation) error {
	if r == nil {
		return errors.New("fairqueue: nil Rabbit disaster repair")
	}
	if ctx == nil {
		return errors.New("fairqueue: nil Rabbit repair context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// External safety confirmations are checked before even the recoverable raw
	// Redis lock mutation. Dry-run remains the only path without both facts.
	if err := attestation.Validate(); err != nil {
		return err
	}
	var begin rabbitRepairBegin
	err := r.journal.WithStartFence(ctx, r.config.Key, r.writer.Fingerprint,
		func(session OperationStartSession) error {
			var beginErr error
			begin, beginErr = r.beginWithStartFence(ctx, session)
			return beginErr
		})
	if err != nil {
		if begin.lock.Validate() == nil {
			r.releaseLockBounded(begin.lock)
		}
		return err
	}
	defer r.releaseLockBounded(begin.lock)
	if begin.terminalComplete {
		return nil
	}
	workCtx, renewal := r.startLockRenewal(ctx, begin.lock)
	defer renewal.stop()
	if err := r.recovery.DrainAttempts(workCtx, r.config); err != nil {
		return renewal.join(fmt.Errorf("fairqueue: drain attempts before Rabbit repair high water: %w", err))
	}
	record, err := r.runRepairPass(workCtx, begin.fence, begin.record)
	if err != nil {
		return renewal.join(err)
	}
	if err := r.recovery.RunRecovery(workCtx, r.config, begin.fence, r.recoverySource); err != nil {
		return renewal.join(fmt.Errorf("fairqueue: rebuild common state after Rabbit repair: %w", err))
	}
	if err := r.renewRecovery(workCtx, begin.fence, "before READY journal commit"); err != nil {
		return renewal.join(err)
	}
	if record.Phase == OperationActive {
		record, err = r.journal.CommitReady(workCtx, record)
		if err != nil {
			return renewal.join(fmt.Errorf("fairqueue: commit Rabbit repair READY journal: %w", err))
		}
		if err := r.validateJournalRecord(record, begin.fence.OperationID, OperationReadyCommitted); err != nil {
			return renewal.join(err)
		}
	}
	if record.Phase != OperationReadyCommitted {
		return renewal.join(fmt.Errorf("%w: Rabbit repair journal is not READY_COMMITTED", ErrInvalidOperationRecord))
	}
	if err := r.renewRecovery(workCtx, begin.fence, "before Redis READY finish"); err != nil {
		return renewal.join(err)
	}
	if err := renewal.stop(); err != nil {
		return err
	}
	readyFence, err := r.recovery.FinishRecovery(ctx, r.config, begin.fence)
	if err != nil {
		return fmt.Errorf("fairqueue: finish Rabbit repair recovery: %w", err)
	}
	if err := readyFence.Validate(); err != nil || readyFence.WriterFingerprint != r.writer.Fingerprint {
		return fmt.Errorf("%w: invalid READY fence after Rabbit repair", ErrCoordinationCorrupt)
	}
	completed, err := r.journal.Complete(ctx, record)
	if err != nil {
		return fmt.Errorf("fairqueue: complete Rabbit repair journal: %w", err)
	}
	if err := r.validateJournalRecord(completed, begin.fence.OperationID, OperationCompleted); err != nil {
		return err
	}
	return nil
}

type rabbitRepairStart struct {
	record           RecoveryOperationRecord
	expected         *RecoveryOperationRecord
	proposal         RecoveryOperationRecord
	terminalComplete bool
}

type rabbitRepairBegin struct {
	lock             RecoveryLock
	fence            RecoveryFence
	record           RecoveryOperationRecord
	terminalComplete bool
}

func (r *RabbitDisasterRepair) beginWithStartFence(
	ctx context.Context,
	session OperationStartSession,
) (rabbitRepairBegin, error) {
	if session == nil {
		return rabbitRepairBegin{}, errors.New("fairqueue: Rabbit repair start session is required")
	}
	owner, err := r.nextIdentity("recovery owner")
	if err != nil {
		return rabbitRepairBegin{}, err
	}
	lock, err := r.coordinator.AcquireRecoveryLock(
		ctx, r.config.Key, owner, r.options.RecoveryLockTTL,
	)
	if err != nil {
		return rabbitRepairBegin{}, fmt.Errorf("fairqueue: acquire Rabbit repair lock: %w", err)
	}
	begin := rabbitRepairBegin{lock: lock}
	if err := r.renewAndCheckStartLock(ctx, lock, "before start preflight"); err != nil {
		return begin, err
	}
	control, err := r.coordinator.InspectRecoveryStart(ctx, r.config.Key, lock)
	if err != nil {
		return begin, fmt.Errorf("fairqueue: inspect Rabbit repair start: %w", err)
	}
	current, found, err := session.Read(ctx)
	if err != nil {
		return begin, fmt.Errorf("fairqueue: read fenced Rabbit repair journal: %w", err)
	}
	start, err := r.planStart(current, found, control)
	if err != nil {
		return begin, err
	}
	if start.terminalComplete {
		if err := r.renewAndCheckStartLock(ctx, lock, "before terminal journal CAS"); err != nil {
			return begin, err
		}
		completed, completeErr := r.journal.Complete(ctx, start.record)
		if completeErr != nil {
			return begin, fmt.Errorf("fairqueue: terminal-complete Rabbit repair journal: %w", completeErr)
		}
		if err := r.coordinator.CheckRecoveryLock(ctx, r.config.Key, lock); err != nil {
			return begin, fmt.Errorf("fairqueue: check Rabbit repair lock after terminal journal CAS: %w", err)
		}
		if err := r.validateJournalRecord(completed, start.record.OperationID, OperationCompleted); err != nil {
			return begin, err
		}
		begin.record = completed
		begin.terminalComplete = true
		return begin, nil
	}

	if err := start.proposal.ValidateProposal(); err != nil {
		return begin, fmt.Errorf("%w: invalid Rabbit repair proposal", ErrInvalidOperationRecord)
	}
	if err := r.renewAndCheckStartLock(ctx, lock, "before ACTIVE journal CAS"); err != nil {
		return begin, err
	}
	record, err := session.BeginSpecial(ctx, start.expected, start.proposal)
	if err != nil {
		return begin, fmt.Errorf("fairqueue: begin Rabbit repair journal: %w", err)
	}
	expectedPhase := OperationActive
	if start.expected != nil && start.expected.Phase == OperationReadyCommitted {
		expectedPhase = OperationReadyCommitted
	}
	if err := r.validateJournalRecord(record, start.proposal.OperationID, expectedPhase); err != nil {
		return begin, err
	}
	if err := r.coordinator.CheckRecoveryLock(ctx, r.config.Key, lock); err != nil {
		return begin, fmt.Errorf("fairqueue: check Rabbit repair lock after ACTIVE journal CAS: %w", err)
	}
	rechecked, err := r.coordinator.InspectRecoveryStart(ctx, r.config.Key, lock)
	if err != nil {
		return begin, fmt.Errorf("fairqueue: recheck Rabbit repair start: %w", err)
	}
	if _, err := r.validateBeginControl(rechecked, record.OperationID, &record); err != nil {
		return begin, err
	}
	fence, err := r.coordinator.BeginRabbitRepairWithLock(
		ctx, r.config.Key, r.writer.Fingerprint, record.OperationID,
		lock, r.options.RecoveryLockTTL,
	)
	if err != nil {
		return begin, fmt.Errorf("fairqueue: begin Rabbit repair recovery: %w", err)
	}
	if err := fence.Validate(); err != nil || fence.Kind != RecoveryRabbitRepair ||
		fence.OperationID != record.OperationID || fence.WriterFingerprint != r.writer.Fingerprint ||
		fence.OwnerToken != lock.OwnerToken {
		return begin, fmt.Errorf("%w: invalid Rabbit repair fence", ErrCoordinationCorrupt)
	}
	begin.fence = fence
	begin.record = record
	return begin, nil
}

func (r *RabbitDisasterRepair) renewAndCheckStartLock(
	ctx context.Context,
	lock RecoveryLock,
	stage string,
) error {
	if err := r.coordinator.RenewRecoveryLock(
		ctx, r.config.Key, lock, r.options.RecoveryLockTTL,
	); err != nil {
		return fmt.Errorf("fairqueue: renew Rabbit repair lock %s: %w", stage, err)
	}
	if err := r.coordinator.CheckRecoveryLock(ctx, r.config.Key, lock); err != nil {
		return fmt.Errorf("fairqueue: check Rabbit repair lock %s: %w", stage, err)
	}
	return nil
}

func (r *RabbitDisasterRepair) nextIdentity(name string) (string, error) {
	if r.tokens == nil {
		return "", fmt.Errorf("fairqueue: Rabbit repair %s source is nil", name)
	}
	value, err := r.tokens.Next()
	if err != nil {
		return "", fmt.Errorf("fairqueue: allocate Rabbit repair %s: %w", name, err)
	}
	if !lowerHex32Pattern.MatchString(value) {
		return "", fmt.Errorf("%w: invalid Rabbit repair %s", ErrInvalidModel, name)
	}
	return value, nil
}

func (r *RabbitDisasterRepair) planStart(
	current RecoveryOperationRecord,
	found bool,
	control RecoveryControlSnapshot,
) (rabbitRepairStart, error) {
	start := rabbitRepairStart{}
	if found {
		if err := current.ValidatePersisted(); err != nil || current.Resource != r.config.Key {
			return start, fmt.Errorf("%w: invalid current Rabbit repair operation", ErrInvalidOperationRecord)
		}
		if current.CurrentWriterFingerprint != r.writer.Fingerprint {
			return start, ErrAuthoritativeWriterMismatch
		}
		if current.Phase != OperationCompleted {
			if current.Kind != RecoveryRabbitRepair {
				return start, operationRequiredf("another special recovery operation is unfinished")
			}
			start.record = current
			start.expected = &start.record
			start.proposal = RecoveryOperationRecord{
				Resource: r.config.Key, OperationID: current.OperationID,
				Kind: RecoveryRabbitRepair, CurrentWriterFingerprint: r.writer.Fingerprint,
			}
			terminal, err := r.validateBeginControl(control, current.OperationID, &current)
			if err != nil {
				return rabbitRepairStart{}, err
			}
			start.terminalComplete = terminal
			return start, nil
		}
		start.expected = &current
	}

	operationID, err := r.nextIdentity("operation ID")
	if err != nil {
		return rabbitRepairStart{}, err
	}
	if found && operationID == current.OperationID {
		return rabbitRepairStart{}, fmt.Errorf("%w: Rabbit repair operation token was reused", ErrCoordinationCorrupt)
	}
	start.proposal = RecoveryOperationRecord{
		Resource: r.config.Key, OperationID: operationID,
		Kind: RecoveryRabbitRepair, CurrentWriterFingerprint: r.writer.Fingerprint,
	}
	if _, err := r.validateBeginControl(control, operationID, nil); err != nil {
		return rabbitRepairStart{}, err
	}
	return start, nil
}

func (r *RabbitDisasterRepair) validateBeginControl(
	control RecoveryControlSnapshot,
	operationID string,
	record *RecoveryOperationRecord,
) (bool, error) {
	phase := OperationPhase("")
	if record != nil {
		phase = record.Phase
	}
	if err := control.Validate(); err != nil {
		return false, fmt.Errorf("%w: invalid Rabbit repair control snapshot",
			errors.Join(ErrCoordinationCorrupt, err))
	}
	if !control.Present {
		if phase == OperationActive || phase == OperationReadyCommitted {
			return false, nil
		}
		return false, operationRequiredf("missing Redis control requires an unfinished Rabbit repair journal")
	}
	if control.WriterFingerprint != r.writer.Fingerprint {
		return false, ErrAuthoritativeWriterMismatch
	}
	switch control.State {
	case ResourceReady:
		if phase == "" && control.LastCompletedOperationID == operationID {
			return false, fmt.Errorf("%w: Rabbit repair operation ID collides with Redis completion history",
				ErrCoordinationCorrupt)
		}
		if phase == OperationReadyCommitted {
			if control.LastCompletedOperationID == operationID {
				return true, nil
			}
			return false, fmt.Errorf("%w: READY control does not attest committed Rabbit repair",
				errors.Join(ErrRecoveryOperatorRequired, ErrResourceNotReady, ErrInvalidRecoveryState))
		}
		if phase == OperationActive && control.LastCompletedOperationID == operationID {
			return false, fmt.Errorf("%w: ACTIVE journal follows completed Redis operation",
				errors.Join(ErrRecoveryOperatorRequired, ErrResourceNotReady, ErrInvalidRecoveryState))
		}
		return false, nil
	case ResourceRecovering:
		if (phase != OperationActive && phase != OperationReadyCommitted) ||
			control.Kind != RecoveryRabbitRepair || control.OperationID != operationID {
			return false, operationRequiredf("Redis is owned by another recovery operation")
		}
		if record == nil || control.Progress == nil || control.Progress.RabbitRepair == nil {
			return false, fmt.Errorf("%w: Rabbit repair progress is missing", ErrCoordinationCorrupt)
		}
		progress := control.Progress.RabbitRepair
		if record.RepairHighWater == nil {
			if progress.RepairHighWater != "" {
				return false, fmt.Errorf("%w: Redis Rabbit repair high water is ahead of its journal", ErrCoordinationCorrupt)
			}
		} else if progress.RepairHighWater != "" && progress.RepairHighWater != *record.RepairHighWater {
			return false, fmt.Errorf("%w: Rabbit repair high water mirror mismatch", ErrCoordinationCorrupt)
		}
		if progress.RepairPassComplete && !record.RepairPassComplete {
			return false, fmt.Errorf("%w: Redis Rabbit repair pass is ahead of its journal", ErrCoordinationCorrupt)
		}
		return false, nil
	default:
		return false, fmt.Errorf("%w: invalid Rabbit repair control state", ErrInvalidRecoveryState)
	}
}

func (r *RabbitDisasterRepair) validateJournalRecord(
	record RecoveryOperationRecord,
	operationID string,
	phase OperationPhase,
) error {
	if err := record.ValidatePersisted(); err != nil || record.Resource != r.config.Key ||
		record.OperationID != operationID || record.Kind != RecoveryRabbitRepair ||
		record.CurrentWriterFingerprint != r.writer.Fingerprint || record.Phase != phase {
		return fmt.Errorf("%w: invalid Rabbit repair journal transition result", ErrInvalidOperationRecord)
	}
	return nil
}

func (r *RabbitDisasterRepair) runRepairPass(
	ctx context.Context,
	fence RecoveryFence,
	record RecoveryOperationRecord,
) (RecoveryOperationRecord, error) {
	if record.RepairHighWater == nil {
		if err := r.renewRecovery(ctx, fence, "before repair high water capture"); err != nil {
			return RecoveryOperationRecord{}, err
		}
		highWater, err := r.broker.CaptureRepairHighWater(ctx)
		if err != nil {
			return RecoveryOperationRecord{}, fmt.Errorf("fairqueue: capture Rabbit repair high water: %w", err)
		}
		if err := ValidateHighWater(highWater); err != nil {
			return RecoveryOperationRecord{}, fmt.Errorf("%w: invalid Rabbit repair high water: %v", ErrInvalidRecoveryState, err)
		}
		if err := r.renewRecovery(ctx, fence, "after repair high water capture"); err != nil {
			return RecoveryOperationRecord{}, err
		}
		record, err = r.journal.SetRepairHighWater(ctx, record, highWater)
		if err != nil {
			return RecoveryOperationRecord{}, fmt.Errorf("fairqueue: journal Rabbit repair high water: %w", err)
		}
		if err := r.validateJournalRecord(record, fence.OperationID, OperationActive); err != nil ||
			record.RepairHighWater == nil || *record.RepairHighWater != highWater || record.RepairPassComplete {
			return RecoveryOperationRecord{}, fmt.Errorf("%w: invalid journaled Rabbit repair high water", ErrInvalidOperationRecord)
		}
	}
	if record.RepairHighWater == nil || ValidateHighWater(*record.RepairHighWater) != nil {
		return RecoveryOperationRecord{}, fmt.Errorf("%w: Rabbit repair journal lacks high water", ErrInvalidOperationRecord)
	}
	if err := r.coordinator.SetRabbitRepairHighWater(
		ctx, r.config.Key, fence, *record.RepairHighWater,
	); err != nil {
		return RecoveryOperationRecord{}, fmt.Errorf("fairqueue: mirror Rabbit repair high water: %w", err)
	}
	if !record.RepairPassComplete {
		pageFence := func(ctx context.Context) error {
			return r.renewRecovery(ctx, fence, "around broker repair page")
		}
		_, _, err := r.scanBrokerCandidates(ctx, *record.RepairHighWater, pageFence,
			func(ctx context.Context, original DispatchCandidate) error {
				updated, changed, err := r.broker.RearmAfterBrokerLoss(ctx, original)
				if err != nil {
					return fmt.Errorf("fairqueue: CAS rearm after broker loss: %w", err)
				}
				if !changed {
					return nil
				}
				if err := r.validateCandidate(updated); err != nil ||
					!sameRabbitRepairIdentity(original.Message, updated.Message) ||
					updated.Message.DispatchToken.Generation <= original.Message.DispatchToken.Generation {
					return fmt.Errorf("%w: invalid broker-loss CAS result", ErrInvalidRecoveryState)
				}
				return nil
			}, pageFence)
		if err != nil {
			return RecoveryOperationRecord{}, err
		}
		updated, err := r.journal.MarkRepairPassComplete(ctx, record)
		if err != nil {
			return RecoveryOperationRecord{}, fmt.Errorf("fairqueue: journal Rabbit repair pass: %w", err)
		}
		if err := r.validateJournalRecord(updated, fence.OperationID, OperationActive); err != nil ||
			!updated.RepairPassComplete || updated.RepairHighWater == nil ||
			*updated.RepairHighWater != *record.RepairHighWater {
			return RecoveryOperationRecord{}, fmt.Errorf("%w: invalid journaled Rabbit repair pass", ErrInvalidOperationRecord)
		}
		record = updated
	}
	if !record.RepairPassComplete {
		return RecoveryOperationRecord{}, fmt.Errorf("%w: Rabbit repair pass is incomplete", ErrInvalidOperationRecord)
	}
	if err := r.coordinator.MarkRabbitRepairPassComplete(ctx, r.config.Key, fence); err != nil {
		return RecoveryOperationRecord{}, fmt.Errorf("fairqueue: mirror Rabbit repair pass: %w", err)
	}
	return record, nil
}

func sameRabbitRepairIdentity(original, updated Message) bool {
	return updated.Version == original.Version &&
		updated.Resource == original.Resource &&
		updated.TenantID == original.TenantID &&
		updated.TaskType == original.TaskType &&
		updated.TaskID == original.TaskID &&
		updated.DispatchToken.Resource == original.DispatchToken.Resource &&
		updated.DispatchToken.TaskID == original.DispatchToken.TaskID
}

func (r *RabbitDisasterRepair) renewRecovery(
	ctx context.Context,
	fence RecoveryFence,
	stage string,
) error {
	if err := r.coordinator.RenewRecovery(
		ctx, r.config.Key, fence, r.options.RecoveryLockTTL,
	); err != nil {
		return fmt.Errorf("fairqueue: renew Rabbit repair recovery %s: %w", stage, err)
	}
	return nil
}

func (r *RabbitDisasterRepair) releaseLockBounded(lock RecoveryLock) {
	if lock.Validate() != nil {
		return
	}
	timeout := r.options.LockRenewInterval
	if timeout > 5*time.Second {
		timeout = 5 * time.Second
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = r.coordinator.ReleaseRecoveryLock(cleanupCtx, r.config.Key, lock)
}

type rabbitRepairRenewal struct {
	cancel context.CancelFunc
	done   chan struct{}
	errCh  chan error
	once   sync.Once
	err    error
}

func (r *RabbitDisasterRepair) startLockRenewal(
	ctx context.Context,
	lock RecoveryLock,
) (context.Context, *rabbitRepairRenewal) {
	workCtx, cancel := context.WithCancel(ctx)
	renewal := &rabbitRepairRenewal{
		cancel: cancel, done: make(chan struct{}), errCh: make(chan error, 1),
	}
	go func() {
		defer close(renewal.done)
		ticker := time.NewTicker(r.options.LockRenewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				if err := r.coordinator.RenewRecoveryLock(
					workCtx, r.config.Key, lock, r.options.RecoveryLockTTL,
				); err != nil {
					renewal.errCh <- err
					cancel()
					return
				}
			}
		}
	}()
	return workCtx, renewal
}

func (r *rabbitRepairRenewal) stop() error {
	if r == nil {
		return nil
	}
	r.once.Do(func() {
		r.cancel()
		<-r.done
		select {
		case r.err = <-r.errCh:
		default:
		}
	})
	return r.err
}

func (r *rabbitRepairRenewal) join(err error) error {
	return errors.Join(err, r.stop())
}
