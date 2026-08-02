package fairqueue

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

const (
	operatorTestWriterOld = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	operatorTestWriterNew = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	operatorTestOperation = "11111111111111111111111111111111"
	operatorTestOwner     = "22222222222222222222222222222222"
	operatorTestNewOp     = "33333333333333333333333333333333"
	operatorTestEpoch     = "44444444444444444444444444444444"
)

type operatorTestEvents struct{ values []string }

func (e *operatorTestEvents) add(value string) { e.values = append(e.values, value) }

func (e *operatorTestEvents) index(value string) int {
	for index, item := range e.values {
		if item == value {
			return index
		}
	}
	return -1
}

func (e *operatorTestEvents) count(value string) int {
	count := 0
	for _, item := range e.values {
		if item == value {
			count++
		}
	}
	return count
}

func (e *operatorTestEvents) requireOrder(t *testing.T, values ...string) {
	t.Helper()
	previous := -1
	for _, value := range values {
		index := e.index(value)
		if index < 0 || index <= previous {
			t.Fatalf("events = %v, want ordered %v", e.values, values)
		}
		previous = index
	}
}

type operatorTestTokens struct {
	values []string
	index  int
}

func (s *operatorTestTokens) Next() (string, error) {
	if s.index >= len(s.values) {
		return "", errors.New("operator test token source exhausted")
	}
	value := s.values[s.index]
	s.index++
	return value, nil
}

func operatorTestRecord(kind RecoveryKind, phase OperationPhase, writer string) RecoveryOperationRecord {
	now := time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC)
	record := RecoveryOperationRecord{
		Resource: "rag.index", OperationID: operatorTestOperation, Kind: kind, Phase: phase,
		CurrentWriterFingerprint: writer, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	switch kind {
	case RecoveryWriterRebind:
		record.OriginalWriterFingerprint = operatorTestWriterOld
		record.TargetWriterFingerprint = operatorTestWriterNew
	case RecoveryForceRebuild:
		notBefore := now.Add(time.Minute)
		record.ForceNotBefore = &notBefore
		if phase != OperationActive {
			record.ForceDeletePassComplete = true
		}
	case RecoveryRabbitRepair:
		if phase != OperationActive {
			highWater := "100"
			record.RepairHighWater = &highWater
			record.RepairPassComplete = true
		}
	}
	return record
}

type operatorTestJournal struct {
	events *operatorTestEvents
	record RecoveryOperationRecord
	found  bool
	begins int
}

func (j *operatorTestJournal) Read(context.Context, string, string) (RecoveryOperationRecord, bool, error) {
	j.events.add("journal.read")
	return j.record, j.found, nil
}

func (j *operatorTestJournal) WithStartFence(
	ctx context.Context,
	resource, writer string,
	fn func(OperationStartSession) error,
) error {
	j.events.add("journal.fence")
	return fn(&operatorTestStartSession{journal: j})
}

func (j *operatorTestJournal) SetRepairHighWater(context.Context, RecoveryOperationRecord, string) (RecoveryOperationRecord, error) {
	panic("unexpected SetRepairHighWater")
}

func (j *operatorTestJournal) MarkRepairPassComplete(context.Context, RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	panic("unexpected MarkRepairPassComplete")
}

func (j *operatorTestJournal) MarkForceDeletePassComplete(_ context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	j.events.add("journal.mark_force")
	j.record = expected
	j.record.ForceDeletePassComplete = true
	j.record.Version++
	j.found = true
	return j.record, nil
}

func (j *operatorTestJournal) CommitReady(_ context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	j.events.add("journal.commit")
	if expected.Kind == RecoveryForceRebuild && !expected.ForceDeletePassComplete {
		return RecoveryOperationRecord{}, ErrInvalidOperationRecord
	}
	j.record = expected
	j.record.Phase = OperationReadyCommitted
	j.record.Version++
	j.found = true
	return j.record, nil
}

func (j *operatorTestJournal) Complete(_ context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	j.events.add("journal.complete")
	j.record = expected
	j.record.Phase = OperationCompleted
	j.record.Version++
	j.found = true
	return j.record, nil
}

type operatorTestStartSession struct{ journal *operatorTestJournal }

func (s *operatorTestStartSession) Read(context.Context) (RecoveryOperationRecord, bool, error) {
	s.journal.events.add("session.read")
	return s.journal.record, s.journal.found, nil
}

func (s *operatorTestStartSession) BeginSpecial(
	_ context.Context,
	expected *RecoveryOperationRecord,
	proposal RecoveryOperationRecord,
) (RecoveryOperationRecord, error) {
	s.journal.events.add("journal.begin")
	s.journal.begins++
	if expected != nil && expected.Phase != OperationCompleted {
		return *expected, nil
	}
	now := time.Date(2026, 8, 2, 2, 0, 0, 0, time.UTC)
	s.journal.record = proposal
	s.journal.record.Phase = OperationActive
	s.journal.record.Version = 1
	s.journal.record.CreatedAt = now
	s.journal.record.UpdatedAt = now
	s.journal.found = true
	return s.journal.record, nil
}

type operatorTestCoordinator struct {
	Coordinator
	events           *operatorTestEvents
	control          RecoveryControlSnapshot
	checkCount       int
	checkFailAt      int
	beginCount       int
	deadlineNow      time.Time
	deadlineCalls    int
	ownedCycles      int
	deleteErr        error
	renewFenceCount  int
	renewFenceFailAt int
}

func (c *operatorTestCoordinator) AcquireRecoveryLock(_ context.Context, _ string, owner string, _ time.Duration) (RecoveryLock, error) {
	c.events.add("coord.acquire")
	return RecoveryLock{OwnerToken: owner}, nil
}

func (c *operatorTestCoordinator) CheckRecoveryLock(context.Context, string, RecoveryLock) error {
	c.events.add("coord.check")
	c.checkCount++
	if c.checkFailAt > 0 && c.checkCount == c.checkFailAt {
		return ErrRecoveryOwnerStale
	}
	return nil
}

func (c *operatorTestCoordinator) RenewRecoveryLock(context.Context, string, RecoveryLock, time.Duration) error {
	c.events.add("coord.renew")
	return nil
}

func (c *operatorTestCoordinator) RenewRecovery(context.Context, string, RecoveryFence, time.Duration) error {
	c.events.add("coord.renew_fence")
	c.renewFenceCount++
	if c.renewFenceFailAt > 0 && c.renewFenceCount == c.renewFenceFailAt {
		return ErrFenceMismatch
	}
	return nil
}

func (c *operatorTestCoordinator) InspectRecoveryStart(context.Context, string, RecoveryLock) (RecoveryControlSnapshot, error) {
	c.events.add("coord.inspect")
	return c.control, nil
}

func (c *operatorTestCoordinator) ComputeForceRebuildDeadlineWithLock(
	_ context.Context,
	_ string,
	_ RecoveryLock,
	minimumDelay time.Duration,
) (ForceRebuildDeadline, error) {
	c.events.add("coord.deadline")
	c.deadlineCalls++
	observed := c.deadlineNow
	if c.deadlineCalls > 1 {
		observed = observed.Add(minimumDelay)
	}
	return NewForceRebuildDeadline(observed, minimumDelay)
}

func (c *operatorTestCoordinator) ReleaseRecoveryLock(context.Context, string, RecoveryLock) error {
	c.events.add("coord.release")
	return nil
}

func (c *operatorTestCoordinator) BeginWriterRebindWithLock(
	_ context.Context,
	_ string,
	_, target, operationID string,
	lock RecoveryLock,
	_ time.Duration,
) (RecoveryFence, error) {
	c.events.add("coord.begin_writer")
	c.beginCount++
	return RecoveryFence{
		ResourceFence: ResourceFence{Epoch: operatorTestEpoch, WriterFingerprint: target},
		OwnerToken:    lock.OwnerToken, Kind: RecoveryWriterRebind, OperationID: operationID,
	}, nil
}

func (c *operatorTestCoordinator) BeginForceRebuildWithLock(
	_ context.Context,
	_ string,
	writer, operationID string,
	_ int64,
	lock RecoveryLock,
	_ time.Duration,
) (RecoveryFence, error) {
	c.events.add("coord.begin_force")
	c.beginCount++
	return RecoveryFence{
		ResourceFence: ResourceFence{Epoch: operatorTestEpoch, WriterFingerprint: writer},
		OwnerToken:    lock.OwnerToken, Kind: RecoveryForceRebuild, OperationID: operationID,
	}, nil
}

func (c *operatorTestCoordinator) ListOwnedResourceKeys(
	context.Context,
	string,
	RecoveryFence,
	string,
	int,
) (RecoveryPage[RedisKeyRef], error) {
	c.events.add("coord.list_owned")
	c.ownedCycles++
	if c.ownedCycles == 1 {
		return RecoveryPage[RedisKeyRef]{Items: []RedisKeyRef{{Key: "fq:{rag.index}:known"}}, Done: true}, nil
	}
	return RecoveryPage[RedisKeyRef]{Done: true}, nil
}

func (c *operatorTestCoordinator) DeleteOwnedResourceKeys(context.Context, string, RecoveryFence, []RedisKeyRef) error {
	c.events.add("coord.delete_owned")
	return c.deleteErr
}

func (c *operatorTestCoordinator) MarkForceDeletePassComplete(context.Context, string, RecoveryFence) error {
	c.events.add("coord.mark_force")
	return nil
}

type operatorTestRunner struct{ events *operatorTestEvents }

func (r *operatorTestRunner) RunRecovery(context.Context, ResourceConfig, RecoveryFence, RecoverySource) error {
	r.events.add("runner.run")
	return nil
}

func (r *operatorTestRunner) FinishRecovery(_ context.Context, _ ResourceConfig, fence RecoveryFence) (ResourceFence, error) {
	r.events.add("runner.finish")
	return fence.ResourceFence, nil
}

type operatorTestRecoverySource struct{}

func (operatorTestRecoverySource) CaptureHighWater(context.Context) (string, error) { return "10", nil }

func (operatorTestRecoverySource) ListKnownTenants(context.Context, string, string, int) (RecoveryPage[TenantRef], error) {
	return RecoveryPage[TenantRef]{Done: true}, nil
}

func (operatorTestRecoverySource) ListDispatched(context.Context, string, string, int) (RecoveryPage[DispatchedRef], error) {
	return RecoveryPage[DispatchedRef]{Done: true}, nil
}

func (operatorTestRecoverySource) ListValidRunning(context.Context, string, string, int) (RecoveryPage[RunningLease], error) {
	return RecoveryPage[RunningLease]{Done: true}, nil
}

type operatorTestWriterSource struct {
	operatorTestRecoverySource
	events    *operatorTestEvents
	identity  WriterIdentity
	readiness WriterReadinessReport
	running   int64
}

func (s *operatorTestWriterSource) ReadWriterIdentity(context.Context) (WriterIdentity, error) {
	s.events.add("source.identity")
	return s.identity, nil
}

func (s *operatorTestWriterSource) CheckSchemaAndInvariants(context.Context) (WriterReadinessReport, error) {
	s.events.add("source.readiness")
	return s.readiness, nil
}

func (s *operatorTestWriterSource) CountValidRunning(context.Context) (int64, error) {
	s.events.add("source.running")
	return s.running, nil
}

type operatorTestRedisInspector struct {
	topology RedisTopology
	control  RecoveryControlSnapshot
}

func (i operatorTestRedisInspector) InspectRedisTopology(context.Context) (RedisTopology, error) {
	return i.topology, nil
}

func (i operatorTestRedisInspector) InspectRecoveryControl(context.Context, string) (RecoveryControlSnapshot, error) {
	return i.control, nil
}

type operatorTestCurrentWriter struct {
	writer   WriterIdentity
	verified bool
}

func (v operatorTestCurrentWriter) VerifyCurrentWriter(context.Context, string) (WriterIdentity, bool, error) {
	return v.writer, v.verified, nil
}

type operatorTestRabbitTruth struct{ verified bool }

func (v operatorTestRabbitTruth) VerifyRabbitTruthSource(context.Context, string) (bool, error) {
	return v.verified, nil
}

func operatorTestResourceConfig() ResourceConfig {
	return ResourceConfig{
		Key: "rag.index", ValidateTaskID: func(string) bool { return true },
		LocalWorkers: 1, GlobalConcurrency: 4, PerUserBaseConcurrency: 1,
		PerUserBurstConcurrency: 2, BorrowEnabled: true,
		ReconcileInterval: time.Second, ExpiredRunningSweepInterval: time.Second,
		ReconcilePageSize: 2, ReservationTTL: 10 * time.Second,
		ReservationHeartbeat: time.Second, PrepareTimeout: time.Second,
		ProvisionalTTL: 2 * time.Second, ProcessingTurnTTL: 2 * time.Second,
		RecoveryDrainTimeout: 10 * time.Second, DispatchInterval: time.Second,
		PublishAttemptTimeout: 2 * time.Second,
	}
}

func operatorReadyControl(writer string) RecoveryControlSnapshot {
	return RecoveryControlSnapshot{
		Present: true, State: ResourceReady, Epoch: operatorTestEpoch,
		ProtocolVersion: ProtocolVersion, WriterFingerprint: writer, Kind: RecoveryNone,
	}
}

func operatorNormalControl(writer string) RecoveryControlSnapshot {
	return RecoveryControlSnapshot{
		Present: true, State: ResourceRecovering, Epoch: operatorTestEpoch,
		ProtocolVersion: ProtocolVersion, WriterFingerprint: writer, Kind: RecoveryNormal,
		Progress: &RecoveryProgress{Kind: RecoveryNormal},
	}
}

func operatorWriterControl(operationID string) RecoveryControlSnapshot {
	return RecoveryControlSnapshot{
		Present: true, State: ResourceRecovering, Epoch: operatorTestEpoch,
		ProtocolVersion: ProtocolVersion, WriterFingerprint: operatorTestWriterNew,
		Kind: RecoveryWriterRebind, OperationID: operationID,
		Progress: &RecoveryProgress{
			Kind: RecoveryWriterRebind, OperationID: operationID,
			WriterRebind: &WriterRebindProgress{
				OriginalWriterFingerprint: operatorTestWriterOld,
				TargetWriterFingerprint:   operatorTestWriterNew,
			},
		},
	}
}

func operatorForceControl(record RecoveryOperationRecord) RecoveryControlSnapshot {
	return RecoveryControlSnapshot{
		Present: true, State: ResourceRecovering, Epoch: operatorTestEpoch,
		ProtocolVersion: ProtocolVersion, WriterFingerprint: operatorTestWriterNew,
		Kind: RecoveryForceRebuild, OperationID: record.OperationID,
		Progress: &RecoveryProgress{
			Kind: RecoveryForceRebuild, OperationID: record.OperationID,
			ForceRebuild: &ForceRebuildProgress{
				NotBeforeUnixMS:    record.ForceNotBefore.UnixMilli(),
				DeletePassComplete: record.ForceDeletePassComplete,
			},
		},
	}
}

func operatorTestOptions() RecoveryOperatorOptions {
	config := operatorTestResourceConfig()
	return RecoveryOperatorOptions{
		ResourceConfig: config, RecoveryLockTTL: 10 * time.Second,
		RecoveryLockRenewInterval: time.Second, ForceRebuildMinimumDelay: config.RecoveryDrainTimeout,
	}
}

func newOperatorTestFixture(t *testing.T, control RecoveryControlSnapshot) (*RecoveryOperators, *operatorTestCoordinator, *operatorTestJournal, *operatorTestRunner, *operatorTestWriterSource, *operatorTestEvents) {
	t.Helper()
	events := &operatorTestEvents{}
	coordinator := &operatorTestCoordinator{
		events: events, control: control,
		deadlineNow: time.Date(2026, 8, 2, 3, 0, 0, 0, time.UTC),
	}
	journal := &operatorTestJournal{events: events}
	runner := &operatorTestRunner{events: events}
	source := &operatorTestWriterSource{
		operatorTestRecoverySource: operatorTestRecoverySource{}, events: events,
		identity: WriterIdentity{Fingerprint: operatorTestWriterNew},
		readiness: WriterReadinessReport{
			Writer: WriterIdentity{Fingerprint: operatorTestWriterNew}, SchemaReady: true,
		},
	}
	operators, err := NewRecoveryOperators(
		coordinator, runner, journal,
		operatorTestRedisInspector{
			topology: RedisTopology{Mode: RedisDeploymentStandalone, WritablePrimary: true},
			control:  control,
		},
		operatorTestCurrentWriter{writer: WriterIdentity{Fingerprint: operatorTestWriterNew}, verified: true},
		operatorTestRabbitTruth{verified: true}, operatorTestOptions(),
	)
	if err != nil {
		t.Fatalf("NewRecoveryOperators() error = %v", err)
	}
	operators.tokens = &operatorTestTokens{values: []string{operatorTestOwner, operatorTestNewOp}}
	return operators, coordinator, journal, runner, source, events
}

func writerAttestation() WriterRebindAttestation {
	return WriterRebindAttestation{
		OldWriterFenced: true, ResourceRuntimesStopped: true, NewWriterAuthoritative: true,
	}
}

func TestWriterRebindApplyUsesStartFenceAndJournalCompletionOrder(t *testing.T) {
	operators, coordinator, journal, _, source, events := newOperatorTestFixture(t, operatorReadyControl(operatorTestWriterOld))
	if err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, writerAttestation(), source); err != nil {
		t.Fatalf("ApplyWriterRebind() error = %v", err)
	}
	if journal.record.Phase != OperationCompleted || journal.record.OperationID != operatorTestNewOp {
		t.Fatalf("journal record = %+v, want completed new operation", journal.record)
	}
	if coordinator.beginCount != 1 {
		t.Fatalf("begin count = %d, want 1", coordinator.beginCount)
	}
	if events.count("coord.renew") < 3 {
		t.Fatalf("events = %v, want start, pre-commit, and pre-Finish lock renewals", events.values)
	}
	if events.count("coord.renew_fence") != 2 {
		t.Fatalf("events = %v, want two complete recovery-fence renewals", events.values)
	}
	events.requireOrder(t,
		"journal.fence", "coord.acquire", "coord.inspect", "session.read",
		"coord.renew", "journal.begin", "coord.begin_writer", "runner.run",
		"journal.commit", "runner.finish", "journal.complete",
	)
	if events.index("coord.release") >= 0 {
		t.Fatalf("events = %v, raw lock must be retained after Begin", events.values)
	}
}

func TestWriterRebindMissingAttestationHasZeroDependencyCalls(t *testing.T) {
	operators, _, journal, _, source, events := newOperatorTestFixture(t, operatorReadyControl(operatorTestWriterOld))
	err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, WriterRebindAttestation{}, source)
	if !errors.Is(err, ErrOperatorConfirmation) {
		t.Fatalf("ApplyWriterRebind() error = %v, want ErrOperatorConfirmation", err)
	}
	if len(events.values) != 0 || journal.begins != 0 {
		t.Fatalf("events = %v, journal begins = %d; want zero calls", events.values, journal.begins)
	}
}

func TestWriterRebindDisallowedStateRejectsBeforeJournalMutation(t *testing.T) {
	operators, coordinator, journal, _, source, events := newOperatorTestFixture(t, operatorReadyControl(operatorTestWriterNew))
	err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, writerAttestation(), source)
	if !errors.Is(err, ErrRecoveryOperatorRequired) {
		t.Fatalf("ApplyWriterRebind() error = %v, want ErrRecoveryOperatorRequired", err)
	}
	if !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("ApplyWriterRebind() error = %v, want fail-closed ErrResourceNotReady classification", err)
	}
	if journal.begins != 0 || coordinator.beginCount != 0 {
		t.Fatalf("journal begins = %d, Redis begins = %d; want zero", journal.begins, coordinator.beginCount)
	}
	if events.index("coord.release") < 0 {
		t.Fatalf("events = %v, rejected pre-Begin start must release its raw lock", events.values)
	}
}

func TestWriterRebindResumesActiveJournalBeforeBeginCrash(t *testing.T) {
	operators, coordinator, journal, _, source, _ := newOperatorTestFixture(t, operatorReadyControl(operatorTestWriterOld))
	journal.record = operatorTestRecord(RecoveryWriterRebind, OperationActive, operatorTestWriterNew)
	journal.found = true
	operators.tokens = &operatorTestTokens{values: []string{operatorTestOwner}}
	if err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, writerAttestation(), source); err != nil {
		t.Fatalf("ApplyWriterRebind(resume) error = %v", err)
	}
	if coordinator.beginCount != 1 || journal.record.OperationID != operatorTestOperation || journal.record.Phase != OperationCompleted {
		t.Fatalf("begin=%d record=%+v, want resumed original operation", coordinator.beginCount, journal.record)
	}
}

func TestWriterRebindTerminalReconcileCompletesWithoutBegin(t *testing.T) {
	control := operatorReadyControl(operatorTestWriterNew)
	control.LastCompletedOperationID = operatorTestOperation
	operators, coordinator, journal, _, source, events := newOperatorTestFixture(t, control)
	journal.record = operatorTestRecord(RecoveryWriterRebind, OperationReadyCommitted, operatorTestWriterNew)
	journal.found = true
	operators.tokens = &operatorTestTokens{values: []string{operatorTestOwner}}
	if err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, writerAttestation(), source); err != nil {
		t.Fatalf("ApplyWriterRebind(terminal) error = %v", err)
	}
	if coordinator.beginCount != 0 || events.index("runner.run") >= 0 || journal.record.Phase != OperationCompleted {
		t.Fatalf("events=%v begin=%d record=%+v, want Complete-only reconcile", events.values, coordinator.beginCount, journal.record)
	}
	events.requireOrder(t, "journal.fence", "coord.acquire", "session.read", "journal.complete", "coord.release")
}

func TestWriterRebindRawLockLossAfterJournalCASLeavesRecoverableActive(t *testing.T) {
	operators, coordinator, journal, _, source, events := newOperatorTestFixture(t, operatorReadyControl(operatorTestWriterOld))
	coordinator.checkFailAt = 3
	err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, writerAttestation(), source)
	if !errors.Is(err, ErrRecoveryOwnerStale) {
		t.Fatalf("ApplyWriterRebind() error = %v, want ErrRecoveryOwnerStale", err)
	}
	if journal.record.Phase != OperationActive || coordinator.beginCount != 0 || events.index("runner.run") >= 0 {
		t.Fatalf("record=%+v begin=%d events=%v, want ACTIVE and zero Begin/business work", journal.record, coordinator.beginCount, events.values)
	}
}

func TestWriterRebindFenceChangeBeforeCommitLeavesJournalActive(t *testing.T) {
	operators, coordinator, journal, _, source, events := newOperatorTestFixture(t, operatorReadyControl(operatorTestWriterOld))
	coordinator.renewFenceFailAt = 1
	err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, writerAttestation(), source)
	if !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("ApplyWriterRebind() error = %v, want ErrFenceMismatch", err)
	}
	if journal.record.Phase != OperationActive || events.index("journal.commit") >= 0 || events.index("runner.finish") >= 0 {
		t.Fatalf("record=%+v events=%v, want zero commit/READY after fence change", journal.record, events.values)
	}
}

func TestWriterRebindFenceChangeBeforeFinishStopsAtReadyCommitted(t *testing.T) {
	operators, coordinator, journal, _, source, events := newOperatorTestFixture(t, operatorReadyControl(operatorTestWriterOld))
	coordinator.renewFenceFailAt = 2
	err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, writerAttestation(), source)
	if !errors.Is(err, ErrFenceMismatch) {
		t.Fatalf("ApplyWriterRebind() error = %v, want ErrFenceMismatch", err)
	}
	if journal.record.Phase != OperationReadyCommitted || events.index("runner.finish") >= 0 || events.index("journal.complete") >= 0 {
		t.Fatalf("record=%+v events=%v, want recoverable READY_COMMITTED and no Redis READY", journal.record, events.values)
	}
}

func TestWriterRebindTakeoverReusesOperationIdentity(t *testing.T) {
	record := operatorTestRecord(RecoveryWriterRebind, OperationActive, operatorTestWriterNew)
	operators, coordinator, journal, _, source, _ := newOperatorTestFixture(t, operatorWriterControl(record.OperationID))
	journal.record = record
	journal.found = true
	operators.tokens = &operatorTestTokens{values: []string{operatorTestOwner}}
	if err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, writerAttestation(), source); err != nil {
		t.Fatalf("ApplyWriterRebind(takeover) error = %v", err)
	}
	if coordinator.beginCount != 1 || journal.record.OperationID != operatorTestOperation || journal.record.Phase != OperationCompleted {
		t.Fatalf("begin=%d record=%+v, want same-operation takeover", coordinator.beginCount, journal.record)
	}
}

func TestWriterRebindReadyCommittedRehydratesMissingRedis(t *testing.T) {
	operators, coordinator, journal, _, source, events := newOperatorTestFixture(t, RecoveryControlSnapshot{})
	journal.record = operatorTestRecord(RecoveryWriterRebind, OperationReadyCommitted, operatorTestWriterNew)
	journal.found = true
	operators.tokens = &operatorTestTokens{values: []string{operatorTestOwner}}
	if err := operators.ApplyWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, writerAttestation(), source); err != nil {
		t.Fatalf("ApplyWriterRebind(rehydrate) error = %v", err)
	}
	if coordinator.beginCount != 1 || events.index("runner.run") < 0 || journal.record.Phase != OperationCompleted {
		t.Fatalf("begin=%d events=%v record=%+v, want rehydrate and redo common passes", coordinator.beginCount, events.values, journal.record)
	}
}

func TestRedisForceRebuildRejectsReadyBeforeJournalMutation(t *testing.T) {
	operators, coordinator, journal, _, _, _ := newOperatorTestFixture(t, operatorReadyControl(operatorTestWriterNew))
	err := operators.ApplyRedisForceRebuild(context.Background(), "rag.index",
		ForceRebuildAttestation{DiscardRedisCoordinationState: true}, operatorTestRecoverySource{})
	if !errors.Is(err, ErrRecoveryOperatorRequired) {
		t.Fatalf("ApplyRedisForceRebuild() error = %v, want ErrRecoveryOperatorRequired", err)
	}
	if journal.begins != 0 || coordinator.beginCount != 0 {
		t.Fatalf("journal begins=%d Redis begins=%d, want zero", journal.begins, coordinator.beginCount)
	}
}

func TestRedisForceRebuildMissingAttestationHasZeroDependencyCalls(t *testing.T) {
	operators, _, journal, _, _, events := newOperatorTestFixture(t, operatorNormalControl(operatorTestWriterNew))
	err := operators.ApplyRedisForceRebuild(context.Background(), "rag.index", ForceRebuildAttestation{}, operatorTestRecoverySource{})
	if !errors.Is(err, ErrOperatorConfirmation) {
		t.Fatalf("ApplyRedisForceRebuild() error = %v, want ErrOperatorConfirmation", err)
	}
	if len(events.values) != 0 || journal.begins != 0 {
		t.Fatalf("events=%v journal begins=%d, want zero calls", events.values, journal.begins)
	}
}

func TestCheckRedisForceRebuildIsReadOnly(t *testing.T) {
	operators, _, journal, _, _, events := newOperatorTestFixture(t, operatorNormalControl(operatorTestWriterNew))
	report, err := operators.CheckRedisForceRebuild(context.Background(), "rag.index", operatorTestRecoverySource{})
	if err != nil {
		t.Fatalf("CheckRedisForceRebuild() error = %v", err)
	}
	if !report.ControlPresent || report.ControlState != ResourceRecovering || report.ControlKind != RecoveryNormal ||
		!report.StandaloneRedis || !report.CurrentWriterVerified || !report.RabbitTruthSourceVerified || report.PagesScanned != 3 {
		t.Fatalf("report = %+v", report)
	}
	if journal.begins != 0 || events.index("coord.acquire") >= 0 || events.index("journal.fence") >= 0 {
		t.Fatalf("dry-run events=%v begins=%d, want read-only inspection", events.values, journal.begins)
	}
}

func TestRedisForceRebuildCompletesZeroRemainingDeletePassBeforeReady(t *testing.T) {
	operators, coordinator, journal, _, _, events := newOperatorTestFixture(t, operatorNormalControl(operatorTestWriterNew))
	if err := operators.ApplyRedisForceRebuild(context.Background(), "rag.index",
		ForceRebuildAttestation{DiscardRedisCoordinationState: true}, operatorTestRecoverySource{}); err != nil {
		t.Fatalf("ApplyRedisForceRebuild() error = %v", err)
	}
	if coordinator.ownedCycles != 2 {
		t.Fatalf("owned scan cycles = %d, want delete cycle plus zero-remaining cycle", coordinator.ownedCycles)
	}
	if journal.record.Phase != OperationCompleted || !journal.record.ForceDeletePassComplete {
		t.Fatalf("journal record = %+v, want completed force delete pass", journal.record)
	}
	events.requireOrder(t,
		"coord.begin_force", "coord.delete_owned", "journal.mark_force", "coord.mark_force",
		"runner.run", "journal.commit", "runner.finish", "journal.complete",
	)
}

func TestRedisForceRebuildHalfDeleteStaysActive(t *testing.T) {
	operators, coordinator, journal, _, _, events := newOperatorTestFixture(t, operatorNormalControl(operatorTestWriterNew))
	coordinator.deleteErr = errors.New("delete interrupted")
	err := operators.ApplyRedisForceRebuild(context.Background(), "rag.index",
		ForceRebuildAttestation{DiscardRedisCoordinationState: true}, operatorTestRecoverySource{})
	if err == nil {
		t.Fatal("ApplyRedisForceRebuild() error = nil, want delete interruption")
	}
	if journal.record.Phase != OperationActive || journal.record.ForceDeletePassComplete ||
		events.index("journal.mark_force") >= 0 || events.index("runner.run") >= 0 {
		t.Fatalf("record=%+v events=%v, want recoverable ACTIVE before pass marker", journal.record, events.values)
	}
}

func TestRedisForceRebuildTakeoverReusesJournalDeadline(t *testing.T) {
	record := operatorTestRecord(RecoveryForceRebuild, OperationActive, operatorTestWriterNew)
	operators, coordinator, journal, _, _, _ := newOperatorTestFixture(t, operatorForceControl(record))
	journal.record = record
	journal.found = true
	operators.tokens = &operatorTestTokens{values: []string{operatorTestOwner}}
	if err := operators.ApplyRedisForceRebuild(context.Background(), "rag.index",
		ForceRebuildAttestation{DiscardRedisCoordinationState: true}, operatorTestRecoverySource{}); err != nil {
		t.Fatalf("ApplyRedisForceRebuild(takeover) error = %v", err)
	}
	if coordinator.beginCount != 1 || coordinator.deadlineCalls != 1 ||
		journal.record.OperationID != operatorTestOperation || journal.record.Phase != OperationCompleted {
		t.Fatalf("begin=%d deadline calls=%d record=%+v, want original ID/deadline takeover",
			coordinator.beginCount, coordinator.deadlineCalls, journal.record)
	}
}

func TestRedisForceRebuildReadyCommittedRehydratesMissingRedis(t *testing.T) {
	operators, coordinator, journal, _, _, events := newOperatorTestFixture(t, RecoveryControlSnapshot{})
	journal.record = operatorTestRecord(RecoveryForceRebuild, OperationReadyCommitted, operatorTestWriterNew)
	journal.found = true
	operators.tokens = &operatorTestTokens{values: []string{operatorTestOwner}}
	if err := operators.ApplyRedisForceRebuild(context.Background(), "rag.index",
		ForceRebuildAttestation{DiscardRedisCoordinationState: true}, operatorTestRecoverySource{}); err != nil {
		t.Fatalf("ApplyRedisForceRebuild(rehydrate) error = %v", err)
	}
	if coordinator.beginCount != 1 || events.index("runner.run") < 0 || journal.record.Phase != OperationCompleted {
		t.Fatalf("begin=%d events=%v record=%+v, want rehydrate and redo common passes", coordinator.beginCount, events.values, journal.record)
	}
}

func TestRedisForceRebuildTerminalReconcileCompletesOnlyJournal(t *testing.T) {
	control := operatorReadyControl(operatorTestWriterNew)
	control.LastCompletedOperationID = operatorTestOperation
	operators, coordinator, journal, _, _, events := newOperatorTestFixture(t, control)
	journal.record = operatorTestRecord(RecoveryForceRebuild, OperationReadyCommitted, operatorTestWriterNew)
	journal.found = true
	operators.tokens = &operatorTestTokens{values: []string{operatorTestOwner}}
	if err := operators.ApplyRedisForceRebuild(context.Background(), "rag.index",
		ForceRebuildAttestation{DiscardRedisCoordinationState: true}, operatorTestRecoverySource{}); err != nil {
		t.Fatalf("ApplyRedisForceRebuild(terminal) error = %v", err)
	}
	if coordinator.beginCount != 0 || events.index("runner.run") >= 0 || events.index("coord.list_owned") >= 0 ||
		journal.record.Phase != OperationCompleted {
		t.Fatalf("begin=%d events=%v record=%+v, want journal-only terminal reconcile",
			coordinator.beginCount, events.values, journal.record)
	}
}

func TestValidateNormalRecoveryJournalRequiresSameKindOperator(t *testing.T) {
	active := operatorTestRecord(RecoveryWriterRebind, OperationActive, operatorTestWriterNew)
	if err := ValidateNormalRecoveryJournal(active, true, RecoveryControlSnapshot{}, operatorTestWriterNew); !errors.Is(err, ErrRecoveryOperatorRequired) {
		t.Fatalf("ValidateNormalRecoveryJournal(ACTIVE) error = %v", err)
	}
	readyCommitted := operatorTestRecord(RecoveryWriterRebind, OperationReadyCommitted, operatorTestWriterNew)
	control := operatorReadyControl(operatorTestWriterNew)
	if err := ValidateNormalRecoveryJournal(readyCommitted, true, control, operatorTestWriterNew); !errors.Is(err, ErrRecoveryOperatorRequired) {
		t.Fatalf("ValidateNormalRecoveryJournal(unmatched READY_COMMITTED) error = %v", err)
	}
	control.LastCompletedOperationID = operatorTestOperation
	if err := ValidateNormalRecoveryJournal(readyCommitted, true, control, operatorTestWriterNew); err != nil {
		t.Fatalf("ValidateNormalRecoveryJournal(matched terminal) error = %v", err)
	}
	completed := operatorTestRecord(RecoveryWriterRebind, OperationCompleted, operatorTestWriterNew)
	if err := ValidateNormalRecoveryJournal(completed, true, RecoveryControlSnapshot{}, operatorTestWriterNew); err != nil {
		t.Fatalf("ValidateNormalRecoveryJournal(COMPLETED) error = %v", err)
	}
	if err := ValidateNormalRecoveryJournal(completed, true, operatorReadyControl(operatorTestWriterNew), operatorTestWriterNew); err != nil {
		t.Fatalf("ValidateNormalRecoveryJournal(COMPLETED with newer READY) error = %v", err)
	}
}

func TestRecoveryOperatorOptionsRejectUnsafeTiming(t *testing.T) {
	options := operatorTestOptions()
	options.RecoveryLockRenewInterval = options.RecoveryLockTTL
	_, err := NewRecoveryOperators(
		&operatorTestCoordinator{events: &operatorTestEvents{}},
		&operatorTestRunner{events: &operatorTestEvents{}},
		&operatorTestJournal{events: &operatorTestEvents{}},
		operatorTestRedisInspector{topology: RedisTopology{Mode: RedisDeploymentStandalone, WritablePrimary: true}},
		operatorTestCurrentWriter{writer: WriterIdentity{Fingerprint: operatorTestWriterNew}, verified: true},
		operatorTestRabbitTruth{verified: true}, options,
	)
	if err == nil {
		t.Fatal("NewRecoveryOperators() error = nil, want unsafe timing rejection")
	}

	options = operatorTestOptions()
	options.ForceRebuildMinimumDelay = options.ResourceConfig.PublishAttemptTimeout
	_, err = NewRecoveryOperators(
		&operatorTestCoordinator{events: &operatorTestEvents{}},
		&operatorTestRunner{events: &operatorTestEvents{}},
		&operatorTestJournal{events: &operatorTestEvents{}},
		operatorTestRedisInspector{topology: RedisTopology{Mode: RedisDeploymentStandalone, WritablePrimary: true}},
		operatorTestCurrentWriter{writer: WriterIdentity{Fingerprint: operatorTestWriterNew}, verified: true},
		operatorTestRabbitTruth{verified: true}, options,
	)
	if err == nil {
		t.Fatal("NewRecoveryOperators() accepted a force delay shorter than RecoveryDrainTimeout")
	}
}

func TestCheckWriterRebindReportsUnsafeFactsWithoutMutation(t *testing.T) {
	events := &operatorTestEvents{}
	journal := &operatorTestJournal{events: events}
	source := &operatorTestWriterSource{
		operatorTestRecoverySource: operatorTestRecoverySource{}, events: events,
		identity:  WriterIdentity{Fingerprint: operatorTestWriterNew},
		readiness: WriterReadinessReport{Writer: WriterIdentity{Fingerprint: operatorTestWriterNew}},
		running:   3,
	}
	report, err := CheckWriterRebind(context.Background(), "rag.index", operatorTestWriterOld, source, journal)
	if err != nil {
		t.Fatalf("CheckWriterRebind() error = %v", err)
	}
	if report.Readiness.Ready() || report.ValidRunningCount != 3 || journal.begins != 0 {
		t.Fatalf("report=%+v begins=%d", report, journal.begins)
	}
	for _, event := range events.values {
		if event == "journal.fence" || event == "journal.begin" {
			t.Fatalf("dry-run events = %v, want no fenced mutation", events.values)
		}
	}
}

func TestRecoveryOperatorFixtureSanity(t *testing.T) {
	// Keep fake validation failures legible when model contracts evolve.
	for name, value := range map[string]any{
		"ready control":  operatorReadyControl(operatorTestWriterOld),
		"normal control": operatorNormalControl(operatorTestWriterNew),
		"writer control": operatorWriterControl(operatorTestOperation),
	} {
		validator, ok := value.(interface{ Validate() error })
		if !ok {
			t.Fatalf("%s has no validator", name)
		}
		if err := validator.Validate(); err != nil {
			t.Fatalf("%s validation error = %v (%s)", name, err, fmt.Sprint(value))
		}
	}
}
