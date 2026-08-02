package fairqueue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"time"

	redislib "github.com/redis/go-redis/v9"
)

// ErrRecoveryOperatorRequired is returned when an unfinished special
// operation, or a cross-store state that can only be reconciled by that same
// operation, prevents NORMAL recovery or ordinary runtime startup.
var ErrRecoveryOperatorRequired = errors.New("fairqueue: recovery operator is required")

// CurrentWriterVerifier and RabbitTruthSourceVerifier deliberately live
// outside RecoverySource. MySQL rebuild pages alone cannot attest that the
// selected writer and Rabbit deployment are authoritative truth sources.
type CurrentWriterVerifier interface {
	VerifyCurrentWriter(ctx context.Context, resource string) (WriterIdentity, bool, error)
}

type RabbitTruthSourceVerifier interface {
	VerifyRabbitTruthSource(ctx context.Context, resource string) (bool, error)
}

// RecoveryOperatorRedisInspector adds a side-effect-free control snapshot to
// the topology inspector. Dry-runs must not transiently acquire the recovery
// lock merely to describe current state.
type RecoveryOperatorRedisInspector interface {
	RedisInspector
	InspectRecoveryControl(ctx context.Context, resource string) (RecoveryControlSnapshot, error)
}

// RecoveryOperatorRunner owns the common, fenced Redis rebuild passes. The
// operator retains ownership of the special-operation journal transitions.
type RecoveryOperatorRunner interface {
	RunRecovery(ctx context.Context, config ResourceConfig, fence RecoveryFence, source RecoverySource) error
	FinishRecovery(ctx context.Context, config ResourceConfig, fence RecoveryFence) (ResourceFence, error)
}

type RecoveryOperatorOptions struct {
	ResourceConfig            ResourceConfig
	RecoveryLockTTL           time.Duration
	RecoveryLockRenewInterval time.Duration
	ForceRebuildMinimumDelay  time.Duration
}

func (o RecoveryOperatorOptions) validate() error {
	if err := o.ResourceConfig.Validate(); err != nil {
		return fmt.Errorf("fairqueue: recovery operator resource config: %w", err)
	}
	durations := []struct {
		name  string
		value time.Duration
	}{
		{"recovery lock TTL", o.RecoveryLockTTL},
		{"recovery lock renew interval", o.RecoveryLockRenewInterval},
		{"force rebuild minimum delay", o.ForceRebuildMinimumDelay},
	}
	for _, item := range durations {
		if item.value <= 0 || item.value > maxResourceDuration {
			return fmt.Errorf("fairqueue: operator %s must be in (0,%s]", item.name, maxResourceDuration)
		}
	}
	if o.RecoveryLockRenewInterval >= o.RecoveryLockTTL {
		return errors.New("fairqueue: operator recovery lock renew interval must be shorter than its TTL")
	}
	// Force rebuild has no process-local attempt registry: it can only prove
	// old check-before-RECOVERING work has ended by waiting out the deployment
	// bound. RecoveryDrainTimeout is strictly larger than publish, prepare,
	// provisional, and processing-turn windows by ResourceConfig validation.
	if o.ForceRebuildMinimumDelay < o.ResourceConfig.RecoveryDrainTimeout {
		return errors.New("fairqueue: force rebuild minimum delay must cover the recovery drain timeout")
	}
	return nil
}

type recoveryOperatorTokenSource interface {
	Next() (string, error)
}

type cryptoRecoveryOperatorTokens struct{}

func (cryptoRecoveryOperatorTokens) Next() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

type RecoveryOperators struct {
	coordinator   Coordinator
	runner        RecoveryOperatorRunner
	journal       OperationJournal
	redis         RecoveryOperatorRedisInspector
	currentWriter CurrentWriterVerifier
	rabbitTruth   RabbitTruthSourceVerifier
	options       RecoveryOperatorOptions
	tokens        recoveryOperatorTokenSource
}

func NewRecoveryOperators(
	coordinator Coordinator,
	runner RecoveryOperatorRunner,
	journal OperationJournal,
	redis RecoveryOperatorRedisInspector,
	currentWriter CurrentWriterVerifier,
	rabbitTruth RabbitTruthSourceVerifier,
	options RecoveryOperatorOptions,
) (*RecoveryOperators, error) {
	if coordinator == nil || runner == nil || journal == nil || redis == nil ||
		currentWriter == nil || rabbitTruth == nil {
		return nil, errors.New("fairqueue: recovery operator dependencies are required")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	return &RecoveryOperators{
		coordinator: coordinator, runner: runner, journal: journal, redis: redis,
		currentWriter: currentWriter, rabbitTruth: rabbitTruth, options: options,
		tokens: cryptoRecoveryOperatorTokens{},
	}, nil
}

func operationRequiredf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errors.Join(ErrRecoveryOperatorRequired, ErrResourceNotReady),
		fmt.Sprintf(format, args...))
}

func validateOperatorContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil recovery operator context", ErrInvalidModel)
	}
	return ctx.Err()
}

const recoveryOperatorCleanupTimeout = 5 * time.Second

func (o *RecoveryOperators) releaseRecoveryLock(ctx context.Context, resource string, lock RecoveryLock) error {
	timeout := recoveryOperatorCleanupTimeout
	if o.options.RecoveryLockTTL < timeout {
		timeout = o.options.RecoveryLockTTL
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return o.coordinator.ReleaseRecoveryLock(cleanupCtx, resource, lock)
}

func operationSummary(record RecoveryOperationRecord, found bool, resource, writer string) (OperationSummary, error) {
	if !found {
		return OperationSummary{}, nil
	}
	if err := record.ValidatePersisted(); err != nil || record.Resource != resource ||
		record.CurrentWriterFingerprint != writer {
		return OperationSummary{}, fmt.Errorf("%w: invalid operation journal record", ErrCoordinationCorrupt)
	}
	return OperationSummary{Present: true, Kind: record.Kind, Phase: record.Phase}, nil
}

func readOperation(
	ctx context.Context,
	journal OperationJournal,
	resource, writer string,
) (RecoveryOperationRecord, bool, OperationSummary, error) {
	record, found, err := journal.Read(ctx, resource, writer)
	if err != nil {
		return RecoveryOperationRecord{}, false, OperationSummary{}, err
	}
	summary, err := operationSummary(record, found, resource, writer)
	if err != nil {
		return RecoveryOperationRecord{}, false, OperationSummary{}, err
	}
	return record, found, summary, nil
}

// CheckWriterRebind is read-only. It reports safety facts but does not treat a
// non-ready target as an executable rebind.
func CheckWriterRebind(
	ctx context.Context,
	resource, expectedOld string,
	source WriterRebindSource,
	journal OperationJournal,
) (WriterRebindReport, error) {
	if err := validateOperatorContext(ctx); err != nil {
		return WriterRebindReport{}, err
	}
	if err := ValidateResource(resource); err != nil || source == nil || journal == nil {
		return WriterRebindReport{}, fmt.Errorf("%w: invalid writer rebind inputs", ErrInvalidModel)
	}
	if err := (WriterIdentity{Fingerprint: expectedOld}).Validate(); err != nil {
		return WriterRebindReport{}, err
	}
	target, err := source.ReadWriterIdentity(ctx)
	if err != nil {
		return WriterRebindReport{}, err
	}
	if err := target.Validate(); err != nil || target.Fingerprint == expectedOld {
		return WriterRebindReport{}, fmt.Errorf("%w: invalid writer rebind target", ErrAuthoritativeWriterMismatch)
	}
	readiness, err := source.CheckSchemaAndInvariants(ctx)
	if err != nil {
		return WriterRebindReport{}, err
	}
	if err := readiness.Validate(); err != nil || readiness.Writer != target {
		return WriterRebindReport{}, fmt.Errorf("%w: inconsistent writer readiness", ErrAuthoritativeWriterMismatch)
	}
	running, err := source.CountValidRunning(ctx)
	if err != nil {
		return WriterRebindReport{}, err
	}
	if running < 0 {
		return WriterRebindReport{}, fmt.Errorf("%w: negative valid RUNNING count", ErrInvalidModel)
	}
	_, _, summary, err := readOperation(ctx, journal, resource, target.Fingerprint)
	if err != nil {
		return WriterRebindReport{}, err
	}
	report := WriterRebindReport{
		Resource: resource, ExpectedOldWriterFingerprint: expectedOld,
		TargetWriter: target, Readiness: readiness, ValidRunningCount: running,
		Operation: summary,
	}
	if err := report.Validate(); err != nil {
		return WriterRebindReport{}, err
	}
	return report, nil
}

func (o *RecoveryOperators) CheckWriterRebind(
	ctx context.Context,
	resource, expectedOld string,
	source WriterRebindSource,
) (WriterRebindReport, error) {
	if err := validateOperatorContext(ctx); err != nil {
		return WriterRebindReport{}, err
	}
	if err := o.validateResource(resource); err != nil {
		return WriterRebindReport{}, err
	}
	return CheckWriterRebind(ctx, resource, expectedOld, source, o.journal)
}

func (o *RecoveryOperators) validateResource(resource string) error {
	if o == nil || resource != o.options.ResourceConfig.Key {
		return fmt.Errorf("%w: unregistered recovery operator resource", ErrInvalidModel)
	}
	return nil
}

func (o *RecoveryOperators) nextToken() (string, error) {
	if o == nil || o.tokens == nil {
		return "", fmt.Errorf("%w: missing recovery token source", ErrCoordinationCorrupt)
	}
	token, err := o.tokens.Next()
	if err != nil {
		return "", fmt.Errorf("%w: generate recovery operator token: %v", ErrDependencyUnavailable, err)
	}
	if !lowerHex32Pattern.MatchString(token) {
		return "", fmt.Errorf("%w: invalid recovery operator token", ErrCoordinationCorrupt)
	}
	return token, nil
}

type operatorStartResult struct {
	record   RecoveryOperationRecord
	fence    RecoveryFence
	terminal bool
}

func (o *RecoveryOperators) withStartLocks(
	ctx context.Context,
	resource, expectedWriter string,
	fn func(context.Context, OperationStartSession, RecoveryLock, RecoveryControlSnapshot) (operatorStartResult, error),
) (result operatorStartResult, err error) {
	owner, err := o.nextToken()
	if err != nil {
		return operatorStartResult{}, err
	}
	err = o.journal.WithStartFence(ctx, resource, expectedWriter, func(session OperationStartSession) (callbackErr error) {
		lock, acquireErr := o.coordinator.AcquireRecoveryLock(ctx, resource, owner, o.options.RecoveryLockTTL)
		if acquireErr != nil {
			return acquireErr
		}
		keepLock := false
		defer func() {
			if keepLock {
				return
			}
			if releaseErr := o.releaseRecoveryLock(ctx, resource, lock); releaseErr != nil {
				callbackErr = errors.Join(callbackErr, releaseErr)
			}
		}()
		if checkErr := o.coordinator.CheckRecoveryLock(ctx, resource, lock); checkErr != nil {
			return checkErr
		}
		control, inspectErr := o.coordinator.InspectRecoveryStart(ctx, resource, lock)
		if inspectErr != nil {
			return inspectErr
		}
		if validateErr := control.Validate(); validateErr != nil {
			return fmt.Errorf("%w: invalid recovery start snapshot", ErrCoordinationCorrupt)
		}
		result, callbackErr = fn(ctx, session, lock, control)
		if callbackErr == nil && !result.terminal {
			keepLock = true
		}
		return callbackErr
	})
	if err != nil {
		return operatorStartResult{}, err
	}
	return result, nil
}

func readStartRecord(ctx context.Context, session OperationStartSession, resource, writer string) (RecoveryOperationRecord, bool, error) {
	record, found, err := session.Read(ctx)
	if err != nil {
		return RecoveryOperationRecord{}, false, err
	}
	if _, err := operationSummary(record, found, resource, writer); err != nil {
		return RecoveryOperationRecord{}, false, err
	}
	return record, found, nil
}

func recordUnfinished(record RecoveryOperationRecord, found bool) bool {
	return found && record.Phase != OperationCompleted
}

func controlTerminal(control RecoveryControlSnapshot, writer, operationID string, kind RecoveryKind) bool {
	return control.Present && control.State == ResourceReady && control.Kind == RecoveryNone &&
		control.WriterFingerprint == writer && control.LastCompletedOperationID == operationID &&
		control.LastCompletedOperationKind == kind
}

func completeTerminal(
	ctx context.Context,
	journal OperationJournal,
	record RecoveryOperationRecord,
) (operatorStartResult, error) {
	if record.Phase == OperationReadyCommitted {
		completed, err := journal.Complete(ctx, record)
		if err != nil {
			return operatorStartResult{}, err
		}
		if err := completed.ValidatePersisted(); err != nil || completed.Phase != OperationCompleted ||
			completed.OperationID != record.OperationID {
			return operatorStartResult{}, fmt.Errorf("%w: invalid completed operation", ErrCoordinationCorrupt)
		}
		record = completed
	}
	return operatorStartResult{record: record, terminal: true}, nil
}

func (o *RecoveryOperators) completeTerminalWithLock(
	ctx context.Context,
	resource string,
	lock RecoveryLock,
	record RecoveryOperationRecord,
) (operatorStartResult, error) {
	if record.Phase == OperationReadyCommitted {
		if err := o.coordinator.RenewRecoveryLock(ctx, resource, lock, o.options.RecoveryLockTTL); err != nil {
			return operatorStartResult{}, err
		}
		if err := o.coordinator.CheckRecoveryLock(ctx, resource, lock); err != nil {
			return operatorStartResult{}, err
		}
	}
	return completeTerminal(ctx, o.journal, record)
}

func (o *RecoveryOperators) beginSpecial(
	ctx context.Context,
	session OperationStartSession,
	lock RecoveryLock,
	expected *RecoveryOperationRecord,
	proposal RecoveryOperationRecord,
	begin func(RecoveryOperationRecord) (RecoveryFence, error),
) (operatorStartResult, error) {
	if err := proposal.ValidateProposal(); err != nil {
		return operatorStartResult{}, err
	}
	if err := o.coordinator.RenewRecoveryLock(ctx, proposal.Resource, lock, o.options.RecoveryLockTTL); err != nil {
		return operatorStartResult{}, err
	}
	if err := o.coordinator.CheckRecoveryLock(ctx, proposal.Resource, lock); err != nil {
		return operatorStartResult{}, err
	}
	record, err := session.BeginSpecial(ctx, expected, proposal)
	if err != nil {
		return operatorStartResult{}, err
	}
	if err := record.ValidatePersisted(); err != nil || record.Resource != proposal.Resource ||
		record.OperationID != proposal.OperationID || record.Kind != proposal.Kind ||
		record.CurrentWriterFingerprint != proposal.CurrentWriterFingerprint ||
		record.Phase == OperationCompleted {
		return operatorStartResult{}, fmt.Errorf("%w: invalid operation start CAS result", ErrCoordinationCorrupt)
	}
	if err := o.coordinator.CheckRecoveryLock(ctx, proposal.Resource, lock); err != nil {
		// The ACTIVE journal is intentionally retained for same-kind recovery.
		return operatorStartResult{}, err
	}
	fence, err := begin(record)
	if err != nil {
		return operatorStartResult{}, err
	}
	if err := fence.Validate(); err != nil || fence.Kind != record.Kind ||
		fence.OperationID != record.OperationID || fence.OwnerToken != lock.OwnerToken ||
		fence.WriterFingerprint != record.CurrentWriterFingerprint {
		return operatorStartResult{}, fmt.Errorf("%w: invalid special recovery fence", ErrCoordinationCorrupt)
	}
	return operatorStartResult{record: record, fence: fence}, nil
}

func writerRecordMatches(record RecoveryOperationRecord, expectedOld, target string) bool {
	return record.Kind == RecoveryWriterRebind && record.CurrentWriterFingerprint == target &&
		record.OriginalWriterFingerprint == expectedOld && record.TargetWriterFingerprint == target
}

func writerStartAllowed(
	control RecoveryControlSnapshot,
	record RecoveryOperationRecord,
	found bool,
	expectedOld, target string,
) (terminal, resume bool, err error) {
	matching := found && writerRecordMatches(record, expectedOld, target)
	if matching && (record.Phase == OperationReadyCommitted || record.Phase == OperationCompleted) &&
		controlTerminal(control, target, record.OperationID, record.Kind) {
		return true, false, nil
	}
	if recordUnfinished(record, found) && !matching {
		return false, false, operationRequiredf("another unfinished operation owns the resource")
	}
	if control.Present && control.State == ResourceReady && control.WriterFingerprint == expectedOld {
		if !found || record.Phase == OperationCompleted {
			return false, false, nil
		}
		if matching && record.Phase == OperationActive {
			if control.LastCompletedOperationID == record.OperationID {
				return false, false, fmt.Errorf("%w: ACTIVE writer journal follows completed Redis operation", ErrCoordinationCorrupt)
			}
			return false, true, nil
		}
		return false, false, operationRequiredf("writer rebind journal cannot resume from READY old-writer control")
	}
	if !control.Present && matching && recordUnfinished(record, found) {
		return false, true, nil
	}
	if control.Present && control.State == ResourceRecovering &&
		control.Kind == RecoveryWriterRebind && matching && recordUnfinished(record, found) &&
		control.WriterFingerprint == target && control.OperationID == record.OperationID {
		if control.Progress == nil || control.Progress.WriterRebind == nil ||
			control.Progress.WriterRebind.OriginalWriterFingerprint != expectedOld ||
			control.Progress.WriterRebind.TargetWriterFingerprint != target {
			return false, false, fmt.Errorf("%w: writer rebind progress mismatch", ErrCoordinationCorrupt)
		}
		return false, true, nil
	}
	return false, false, operationRequiredf("writer rebind start state is not allowed")
}

func (o *RecoveryOperators) recheckWriter(
	ctx context.Context,
	expectedOld, target string,
	source WriterRebindSource,
) error {
	identity, err := source.ReadWriterIdentity(ctx)
	if err != nil {
		return err
	}
	if err := identity.Validate(); err != nil || identity.Fingerprint != target || identity.Fingerprint == expectedOld {
		return fmt.Errorf("%w: writer identity changed during rebind", ErrAuthoritativeWriterMismatch)
	}
	readiness, err := source.CheckSchemaAndInvariants(ctx)
	if err != nil {
		return err
	}
	if readiness.Writer != identity || !readiness.Ready() {
		return fmt.Errorf("%w: target writer is not ready", ErrResourceNotReady)
	}
	running, err := source.CountValidRunning(ctx)
	if err != nil {
		return err
	}
	if running != 0 {
		return fmt.Errorf("%w: target writer still has valid RUNNING tasks", ErrResourceNotReady)
	}
	return nil
}

func (o *RecoveryOperators) startWriterRebind(
	ctx context.Context,
	resource, expectedOld, target string,
	source WriterRebindSource,
) (operatorStartResult, error) {
	return o.withStartLocks(ctx, resource, target,
		func(ctx context.Context, session OperationStartSession, lock RecoveryLock, control RecoveryControlSnapshot) (operatorStartResult, error) {
			record, found, err := readStartRecord(ctx, session, resource, target)
			if err != nil {
				return operatorStartResult{}, err
			}
			if err := o.recheckWriter(ctx, expectedOld, target, source); err != nil {
				return operatorStartResult{}, err
			}
			terminal, resume, err := writerStartAllowed(control, record, found, expectedOld, target)
			if err != nil {
				return operatorStartResult{}, err
			}
			if terminal {
				return o.completeTerminalWithLock(ctx, resource, lock, record)
			}
			operationID := ""
			var expected *RecoveryOperationRecord
			if resume {
				operationID = record.OperationID
				expected = &record
			} else {
				operationID, err = o.nextToken()
				if err != nil {
					return operatorStartResult{}, err
				}
				if found {
					if operationID == record.OperationID {
						return operatorStartResult{}, fmt.Errorf("%w: operation token was reused", ErrCoordinationCorrupt)
					}
					expected = &record
				}
			}
			proposal := RecoveryOperationRecord{
				Resource: resource, OperationID: operationID, Kind: RecoveryWriterRebind,
				CurrentWriterFingerprint: target, OriginalWriterFingerprint: expectedOld,
				TargetWriterFingerprint: target,
			}
			return o.beginSpecial(ctx, session, lock, expected, proposal,
				func(record RecoveryOperationRecord) (RecoveryFence, error) {
					return o.coordinator.BeginWriterRebindWithLock(ctx, resource, expectedOld, target,
						record.OperationID, lock, o.options.RecoveryLockTTL)
				})
		})
}

func (o *RecoveryOperators) finishSpecial(
	ctx context.Context,
	result operatorStartResult,
	source RecoverySource,
	revalidate func(context.Context) error,
) error {
	if result.terminal {
		return nil
	}
	if err := o.runner.RunRecovery(ctx, o.options.ResourceConfig, result.fence, source); err != nil {
		return err
	}
	if revalidate != nil {
		if err := revalidate(ctx); err != nil {
			return err
		}
	}
	lock := RecoveryLock{OwnerToken: result.fence.OwnerToken}
	if err := o.coordinator.RenewRecoveryLock(ctx, result.record.Resource, lock, o.options.RecoveryLockTTL); err != nil {
		return err
	}
	if err := o.coordinator.RenewRecovery(ctx, result.record.Resource, result.fence, o.options.RecoveryLockTTL); err != nil {
		return err
	}
	record, err := o.journal.CommitReady(ctx, result.record)
	if err != nil {
		return err
	}
	if err := record.ValidatePersisted(); err != nil || record.Phase != OperationReadyCommitted ||
		record.OperationID != result.record.OperationID {
		return fmt.Errorf("%w: invalid READY_COMMITTED operation", ErrCoordinationCorrupt)
	}
	if revalidate != nil {
		if err := revalidate(ctx); err != nil {
			return err
		}
	}
	if err := o.coordinator.RenewRecoveryLock(ctx, result.record.Resource, lock, o.options.RecoveryLockTTL); err != nil {
		return err
	}
	if err := o.coordinator.RenewRecovery(ctx, result.record.Resource, result.fence, o.options.RecoveryLockTTL); err != nil {
		return err
	}
	readyFence, err := o.runner.FinishRecovery(ctx, o.options.ResourceConfig, result.fence)
	if err != nil {
		return err
	}
	if err := readyFence.Validate(); err != nil || readyFence.WriterFingerprint != result.fence.WriterFingerprint {
		return fmt.Errorf("%w: invalid READY fence after special recovery", ErrCoordinationCorrupt)
	}
	completed, err := o.journal.Complete(ctx, record)
	if err != nil {
		return err
	}
	if err := completed.ValidatePersisted(); err != nil || completed.Phase != OperationCompleted ||
		completed.OperationID != record.OperationID {
		return fmt.Errorf("%w: invalid COMPLETED operation", ErrCoordinationCorrupt)
	}
	return nil
}

func (o *RecoveryOperators) ApplyWriterRebind(
	ctx context.Context,
	resource, expectedOld string,
	attestation WriterRebindAttestation,
	source WriterRebindSource,
) error {
	if err := validateOperatorContext(ctx); err != nil {
		return err
	}
	if err := o.validateResource(resource); err != nil {
		return err
	}
	// Confirmation is checked before even read-only dependency calls, so a
	// missing flag cannot acquire a recovery lock or mutate the journal.
	if err := attestation.Validate(); err != nil {
		return err
	}
	report, err := CheckWriterRebind(ctx, resource, expectedOld, source, o.journal)
	if err != nil {
		return err
	}
	if !report.Readiness.Ready() || report.ValidRunningCount != 0 {
		return fmt.Errorf("%w: writer rebind preconditions are not satisfied", ErrResourceNotReady)
	}
	result, err := o.startWriterRebind(ctx, resource, expectedOld, report.TargetWriter.Fingerprint, source)
	if err != nil {
		return err
	}
	return o.finishSpecial(ctx, result, source, func(ctx context.Context) error {
		return o.recheckWriter(ctx, expectedOld, report.TargetWriter.Fingerprint, source)
	})
}

type forceSafetyFacts struct {
	writer          WriterIdentity
	current         bool
	rabbit          bool
	standaloneRedis bool
}

func (o *RecoveryOperators) inspectForceSafety(ctx context.Context, resource string) (forceSafetyFacts, error) {
	writer, current, err := o.currentWriter.VerifyCurrentWriter(ctx, resource)
	if err != nil {
		return forceSafetyFacts{}, err
	}
	if err := writer.Validate(); err != nil {
		return forceSafetyFacts{}, fmt.Errorf("%w: invalid current writer inspection", ErrAuthoritativeWriterMismatch)
	}
	rabbit, err := o.rabbitTruth.VerifyRabbitTruthSource(ctx, resource)
	if err != nil {
		return forceSafetyFacts{}, err
	}
	topology, err := o.redis.InspectRedisTopology(ctx)
	if err != nil {
		return forceSafetyFacts{}, err
	}
	if err := topology.Validate(); err != nil {
		return forceSafetyFacts{}, err
	}
	return forceSafetyFacts{
		writer: writer, current: current, rabbit: rabbit,
		standaloneRedis: topology.SupportsFairQueue(),
	}, nil
}

func addReportPage(count, pages *int64, size int) error {
	if size < 0 || *count > math.MaxInt64-int64(size) || *pages == math.MaxInt64 {
		return fmt.Errorf("%w: recovery report aggregate overflow", ErrInvalidModel)
	}
	*count += int64(size)
	*pages++
	return nil
}

func scanRecoverySource(ctx context.Context, source RecoverySource, limit int) (int64, int64, error) {
	highWater, err := source.CaptureHighWater(ctx)
	if err != nil {
		return 0, 0, err
	}
	if err := ValidateHighWater(highWater); err != nil {
		return 0, 0, err
	}
	var count, pages int64
	after := ""
	for {
		page, err := source.ListKnownTenants(ctx, highWater, after, limit)
		if err != nil {
			return 0, 0, err
		}
		if err := page.Validate(after, limit); err != nil {
			return 0, 0, err
		}
		if err := addReportPage(&count, &pages, len(page.Items)); err != nil {
			return 0, 0, err
		}
		if page.Done {
			break
		}
		after = page.NextCursor
	}
	after = ""
	for {
		page, err := source.ListDispatched(ctx, highWater, after, limit)
		if err != nil {
			return 0, 0, err
		}
		if err := page.Validate(after, limit); err != nil {
			return 0, 0, err
		}
		if err := addReportPage(&count, &pages, len(page.Items)); err != nil {
			return 0, 0, err
		}
		if page.Done {
			break
		}
		after = page.NextCursor
	}
	after = ""
	for {
		page, err := source.ListValidRunning(ctx, highWater, after, limit)
		if err != nil {
			return 0, 0, err
		}
		if err := page.Validate(after, limit); err != nil {
			return 0, 0, err
		}
		if err := addReportPage(&count, &pages, len(page.Items)); err != nil {
			return 0, 0, err
		}
		if page.Done {
			break
		}
		after = page.NextCursor
	}
	return count, pages, nil
}

func (o *RecoveryOperators) inspectControlForReport(
	ctx context.Context,
	resource string,
) (RecoveryControlSnapshot, error) {
	snapshot, err := o.redis.InspectRecoveryControl(ctx, resource)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return RecoveryControlSnapshot{}, fmt.Errorf("%w: invalid force rebuild control", ErrCoordinationCorrupt)
	}
	return snapshot, nil
}

func (o *RecoveryOperators) CheckRedisForceRebuild(
	ctx context.Context,
	resource string,
	source RecoverySource,
) (ForceRebuildReport, error) {
	if err := validateOperatorContext(ctx); err != nil {
		return ForceRebuildReport{}, err
	}
	if err := o.validateResource(resource); err != nil || source == nil {
		return ForceRebuildReport{}, fmt.Errorf("%w: invalid force rebuild inputs", ErrInvalidModel)
	}
	facts, err := o.inspectForceSafety(ctx, resource)
	if err != nil {
		return ForceRebuildReport{}, err
	}
	_, _, summary, err := readOperation(ctx, o.journal, resource, facts.writer.Fingerprint)
	if err != nil {
		return ForceRebuildReport{}, err
	}
	control, err := o.inspectControlForReport(ctx, resource)
	if err != nil {
		return ForceRebuildReport{}, err
	}
	count, pages, err := scanRecoverySource(ctx, source, o.options.ResourceConfig.ReconcilePageSize)
	if err != nil {
		return ForceRebuildReport{}, err
	}
	report := ForceRebuildReport{
		Resource: resource, Writer: facts.writer,
		ControlPresent: control.Present, StandaloneRedis: facts.standaloneRedis,
		CurrentWriterVerified: facts.current, RabbitTruthSourceVerified: facts.rabbit,
		RebuildableKeyCount: count, PagesScanned: pages, Operation: summary,
	}
	if control.Present {
		report.ControlState = control.State
		report.ControlKind = control.Kind
	}
	if err := report.Validate(); err != nil {
		return ForceRebuildReport{}, err
	}
	return report, nil
}

func forceRecordMatches(record RecoveryOperationRecord, writer string) bool {
	return record.Kind == RecoveryForceRebuild && record.CurrentWriterFingerprint == writer &&
		validCanonicalForceNotBefore(record.ForceNotBefore)
}

func forceStartAllowed(
	control RecoveryControlSnapshot,
	record RecoveryOperationRecord,
	found bool,
	writer string,
) (terminal, resume bool, err error) {
	matching := found && forceRecordMatches(record, writer)
	if matching && (record.Phase == OperationReadyCommitted || record.Phase == OperationCompleted) &&
		controlTerminal(control, writer, record.OperationID, record.Kind) {
		return true, false, nil
	}
	if recordUnfinished(record, found) && !matching {
		return false, false, operationRequiredf("another unfinished operation owns the resource")
	}
	if control.Present && control.State == ResourceRecovering && control.Kind == RecoveryNormal &&
		control.WriterFingerprint == writer {
		if !found || record.Phase == OperationCompleted {
			return false, false, nil
		}
		if matching && record.Phase == OperationActive {
			return false, true, nil
		}
	}
	if !control.Present && matching && recordUnfinished(record, found) {
		return false, true, nil
	}
	if control.Present && control.State == ResourceRecovering && control.Kind == RecoveryForceRebuild &&
		matching && recordUnfinished(record, found) && control.WriterFingerprint == writer &&
		control.OperationID == record.OperationID {
		if control.Progress == nil || control.Progress.ForceRebuild == nil ||
			control.Progress.ForceRebuild.NotBeforeUnixMS != record.ForceNotBefore.UnixMilli() {
			return false, false, fmt.Errorf("%w: force rebuild progress mismatch", ErrCoordinationCorrupt)
		}
		if control.Progress.ForceRebuild.DeletePassComplete && !record.ForceDeletePassComplete {
			return false, false, fmt.Errorf("%w: Redis force delete marker is ahead of its journal", ErrCoordinationCorrupt)
		}
		return false, true, nil
	}
	return false, false, operationRequiredf("force rebuild start state is not allowed")
}

func (o *RecoveryOperators) recheckForceSafety(
	ctx context.Context,
	resource, writer string,
) error {
	facts, err := o.inspectForceSafety(ctx, resource)
	if err != nil {
		return err
	}
	if facts.writer.Fingerprint != writer || !facts.current {
		return fmt.Errorf("%w: current writer attestation failed", ErrAuthoritativeWriterMismatch)
	}
	if !facts.rabbit {
		return fmt.Errorf("%w: Rabbit truth source is not verified", ErrResourceNotReady)
	}
	if !facts.standaloneRedis {
		return ErrUnsupportedTopology
	}
	return nil
}

func (o *RecoveryOperators) startForceRebuild(
	ctx context.Context,
	resource, writer string,
) (operatorStartResult, error) {
	return o.withStartLocks(ctx, resource, writer,
		func(ctx context.Context, session OperationStartSession, lock RecoveryLock, control RecoveryControlSnapshot) (operatorStartResult, error) {
			record, found, err := readStartRecord(ctx, session, resource, writer)
			if err != nil {
				return operatorStartResult{}, err
			}
			if err := o.recheckForceSafety(ctx, resource, writer); err != nil {
				return operatorStartResult{}, err
			}
			terminal, resume, err := forceStartAllowed(control, record, found, writer)
			if err != nil {
				return operatorStartResult{}, err
			}
			if terminal {
				return o.completeTerminalWithLock(ctx, resource, lock, record)
			}
			operationID := ""
			var notBefore time.Time
			var expected *RecoveryOperationRecord
			if resume {
				operationID = record.OperationID
				notBefore = *record.ForceNotBefore
				expected = &record
			} else {
				operationID, err = o.nextToken()
				if err != nil {
					return operatorStartResult{}, err
				}
				if found {
					if operationID == record.OperationID {
						return operatorStartResult{}, fmt.Errorf("%w: operation token was reused", ErrCoordinationCorrupt)
					}
					expected = &record
				}
				deadline, deadlineErr := o.coordinator.ComputeForceRebuildDeadlineWithLock(
					ctx, resource, lock, o.options.ForceRebuildMinimumDelay)
				if deadlineErr != nil {
					return operatorStartResult{}, deadlineErr
				}
				if err := deadline.Validate(o.options.ForceRebuildMinimumDelay); err != nil {
					return operatorStartResult{}, err
				}
				notBefore = deadline.NotBefore
			}
			proposal := RecoveryOperationRecord{
				Resource: resource, OperationID: operationID, Kind: RecoveryForceRebuild,
				CurrentWriterFingerprint: writer, ForceNotBefore: &notBefore,
			}
			return o.beginSpecial(ctx, session, lock, expected, proposal,
				func(record RecoveryOperationRecord) (RecoveryFence, error) {
					return o.coordinator.BeginForceRebuildWithLock(ctx, resource, writer,
						record.OperationID, record.ForceNotBefore.UnixMilli(), lock, o.options.RecoveryLockTTL)
				})
		})
}

func (o *RecoveryOperators) waitForForceDeadline(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
	notBefore time.Time,
) error {
	for {
		if err := o.coordinator.RenewRecoveryLock(ctx, resource,
			RecoveryLock{OwnerToken: fence.OwnerToken}, o.options.RecoveryLockTTL); err != nil {
			return err
		}
		deadline, err := o.coordinator.ComputeForceRebuildDeadlineWithLock(ctx, resource,
			RecoveryLock{OwnerToken: fence.OwnerToken}, o.options.ForceRebuildMinimumDelay)
		if err != nil {
			return err
		}
		if err := deadline.Validate(o.options.ForceRebuildMinimumDelay); err != nil {
			return err
		}
		if !deadline.ObservedRedisTime.Before(notBefore) {
			return nil
		}
		wait := notBefore.Sub(deadline.ObservedRedisTime)
		if wait > o.options.RecoveryLockRenewInterval {
			wait = o.options.RecoveryLockRenewInterval
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (o *RecoveryOperators) deleteForceOwnedKeys(
	ctx context.Context,
	resource string,
	fence RecoveryFence,
) error {
	limit := o.options.ResourceConfig.ReconcilePageSize
	for {
		foundAny := false
		after := ""
		for {
			if err := o.coordinator.RenewRecoveryLock(ctx, resource,
				RecoveryLock{OwnerToken: fence.OwnerToken}, o.options.RecoveryLockTTL); err != nil {
				return err
			}
			page, err := o.coordinator.ListOwnedResourceKeys(ctx, resource, fence, after, limit)
			if err != nil {
				return err
			}
			if err := page.Validate(after, limit); err != nil {
				return err
			}
			if len(page.Items) > 0 {
				foundAny = true
				if err := o.coordinator.DeleteOwnedResourceKeys(ctx, resource, fence, page.Items); err != nil {
					return err
				}
			}
			if page.Done {
				break
			}
			after = page.NextCursor
		}
		if !foundAny {
			return nil
		}
	}
}

func (o *RecoveryOperators) applyForceDeletePass(
	ctx context.Context,
	resource string,
	result *operatorStartResult,
) error {
	if result == nil || result.terminal || result.record.ForceNotBefore == nil {
		return fmt.Errorf("%w: invalid force rebuild start result", ErrInvalidRecoveryState)
	}
	if err := o.waitForForceDeadline(ctx, resource, result.fence, *result.record.ForceNotBefore); err != nil {
		return err
	}
	if err := o.recheckForceSafety(ctx, resource, result.record.CurrentWriterFingerprint); err != nil {
		return err
	}
	if err := o.deleteForceOwnedKeys(ctx, resource, result.fence); err != nil {
		return err
	}
	if err := o.recheckForceSafety(ctx, resource, result.record.CurrentWriterFingerprint); err != nil {
		return err
	}
	if result.record.Phase == OperationActive {
		record, err := o.journal.MarkForceDeletePassComplete(ctx, result.record)
		if err != nil {
			return err
		}
		if err := record.ValidatePersisted(); err != nil || !record.ForceDeletePassComplete ||
			record.OperationID != result.record.OperationID {
			return fmt.Errorf("%w: invalid force delete journal result", ErrCoordinationCorrupt)
		}
		result.record = record
	}
	return o.coordinator.MarkForceDeletePassComplete(ctx, resource, result.fence)
}

func (o *RecoveryOperators) ApplyRedisForceRebuild(
	ctx context.Context,
	resource string,
	attestation ForceRebuildAttestation,
	source RecoverySource,
) error {
	if err := validateOperatorContext(ctx); err != nil {
		return err
	}
	if err := o.validateResource(resource); err != nil || source == nil {
		return fmt.Errorf("%w: invalid force rebuild inputs", ErrInvalidModel)
	}
	if err := attestation.Validate(); err != nil {
		return err
	}
	facts, err := o.inspectForceSafety(ctx, resource)
	if err != nil {
		return err
	}
	if !facts.current {
		return fmt.Errorf("%w: current writer is not verified", ErrAuthoritativeWriterMismatch)
	}
	if !facts.rabbit {
		return fmt.Errorf("%w: Rabbit truth source is not verified", ErrResourceNotReady)
	}
	if !facts.standaloneRedis {
		return ErrUnsupportedTopology
	}
	result, err := o.startForceRebuild(ctx, resource, facts.writer.Fingerprint)
	if err != nil {
		return err
	}
	if result.terminal {
		return nil
	}
	if err := o.applyForceDeletePass(ctx, resource, &result); err != nil {
		return err
	}
	return o.finishSpecial(ctx, result, source, func(ctx context.Context) error {
		return o.recheckForceSafety(ctx, resource, result.record.CurrentWriterFingerprint)
	})
}

// ValidateNormalRecoveryJournal is shared with startup recovery. It is pure:
// the caller performs a matching terminal Complete when READY_COMMITTED is
// paired with the exact READY last-completed operation ID.
func ValidateNormalRecoveryJournal(
	record RecoveryOperationRecord,
	found bool,
	control RecoveryControlSnapshot,
	expectedWriter string,
) error {
	if err := control.Validate(); err != nil {
		return fmt.Errorf("%w: invalid recovery control", ErrCoordinationCorrupt)
	}
	if !found {
		return nil
	}
	if err := record.ValidatePersisted(); err != nil || record.CurrentWriterFingerprint != expectedWriter {
		return operationRequiredf("operation journal writer or shape is inconsistent")
	}
	switch record.Phase {
	case OperationActive:
		return operationRequiredf("special operation %s is ACTIVE", record.Kind)
	case OperationReadyCommitted:
		if !controlTerminal(control, expectedWriter, record.OperationID, record.Kind) {
			return operationRequiredf("READY_COMMITTED operation lacks matching READY control")
		}
	case OperationCompleted:
		return nil
	default:
		return operationRequiredf("unknown special operation phase")
	}
	return nil
}

// This Lua program is read-only. It intentionally mirrors the structural
// validation performed by InspectRecoveryStart without requiring or changing
// the raw recovery lock.
var redisInspectRecoveryControlScript = redislib.NewScript(redisRawLockFenceLua + `
if redis.call('EXISTS', KEYS[1]) ~= 1 then
  if redis.call('EXISTS', KEYS[2]) ~= 0 then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  return {'OK', '0'}
end
local control = redis.call('HMGET', KEYS[1],
  'state', 'epoch', 'protocol_version', 'writer_fingerprint',
	  'operation_kind', 'operation_id', 'last_completed_operation_id',
	  'last_completed_operation_kind')
for i = 1, 8 do
  if control[i] == false then return {'FQ_COORDINATION_CORRUPT'} end
end
if not tonumber(control[3]) or tonumber(control[3]) < 1 or
    math.floor(tonumber(control[3])) ~= tonumber(control[3]) or
    not fq_hex(control[2], 32) or not fq_hex(control[4], 64) or
	    (control[7] == '' and control[8] ~= 'NONE') or
	    (control[7] ~= '' and (not fq_hex(control[7], 32) or
	      (control[8] ~= 'RABBIT_REPAIR' and control[8] ~= 'WRITER_REBIND' and
	        control[8] ~= 'FORCE_REBUILD'))) then
  return {'FQ_COORDINATION_CORRUPT'}
end
if control[3] ~= ARGV[1] then return {'FQ_FENCE_MISMATCH'} end
if control[1] == 'READY' then
  if control[5] ~= 'NONE' or control[6] ~= '' or redis.call('EXISTS', KEYS[2]) ~= 0 then
    return {'FQ_COORDINATION_CORRUPT'}
  end
  return {'OK', '1', control[1], control[2], control[3], control[4],
	    control[5], control[6], control[7], control[8]}
end
if control[1] ~= 'RECOVERING' or control[5] == 'NONE' or redis.call('EXISTS', KEYS[2]) ~= 1 then
  return {'FQ_COORDINATION_CORRUPT'}
end
if control[5] ~= 'NORMAL' and control[7] == control[6] then
  return {'FQ_COORDINATION_CORRUPT'}
end
local progress = redis.call('HMGET', KEYS[2],
  'epoch', 'operation_kind', 'operation_id', 'high_water',
  'known_cycle', 'known_complete', 'known_diff',
  'dispatched_cycle', 'dispatched_complete', 'dispatched_diff',
  'running_cycle', 'running_complete', 'running_diff',
  'repair_high_water', 'repair_complete',
  'rebind_original_writer', 'rebind_target_writer',
  'force_not_before_ms', 'force_delete_complete')
for i = 1, 19 do
  if progress[i] == false then return {'FQ_COORDINATION_CORRUPT'} end
end
if progress[1] ~= control[2] or progress[2] ~= control[5] or progress[3] ~= control[6] then
  return {'FQ_COORDINATION_CORRUPT'}
end
local result = {'OK', '1', control[1], control[2], control[3], control[4],
	  control[5], control[6], control[7], control[8]}
for i = 1, 19 do table.insert(result, progress[i]) end
return result
`)

// InspectRecoveryControl is the read-only operator-report counterpart of the
// raw-lock-fenced InspectRecoveryStart method.
func (r *Redis) InspectRecoveryControl(ctx context.Context, resource string) (RecoveryControlSnapshot, error) {
	if r == nil {
		return RecoveryControlSnapshot{}, fmt.Errorf("%w: nil Redis inspector", ErrInvalidModel)
	}
	if err := ValidateResource(resource); err != nil {
		return RecoveryControlSnapshot{}, fmt.Errorf("%w: invalid Redis resource", ErrInvalidModel)
	}
	keys, err := r.resourceKeys(resource)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	opCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	defer cancel()
	result, err := r.runScript(opCtx, "inspect recovery control", redisInspectRecoveryControlScript,
		[]string{keys.control, keys.progress}, fmt.Sprint(ProtocolVersion))
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	values, err := redisArrayResult(result)
	if err != nil {
		return RecoveryControlSnapshot{}, err
	}
	return parseRecoverySnapshot(values)
}

var _ RecoveryOperatorRedisInspector = (*Redis)(nil)
