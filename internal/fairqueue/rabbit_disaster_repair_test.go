package fairqueue

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	rabbitRepairTestWriter = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rabbitRepairTestOwner1 = "11111111111111111111111111111111"
	rabbitRepairTestOwner2 = "22222222222222222222222222222222"
	rabbitRepairTestOp     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type rabbitRepairTestEvents struct {
	mu     sync.Mutex
	values []string
}

func (e *rabbitRepairTestEvents) add(value string) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.values = append(e.values, value)
	e.mu.Unlock()
}

func (e *rabbitRepairTestEvents) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.values...)
}

func rabbitRepairTestIndex(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func rabbitRepairTestBefore(t *testing.T, events []string, first, second string) {
	t.Helper()
	left := rabbitRepairTestIndex(events, first)
	right := rabbitRepairTestIndex(events, second)
	if left < 0 || right < 0 || left >= right {
		t.Fatalf("event order %q before %q not satisfied: %v", first, second, events)
	}
}

type rabbitRepairTestTokens struct {
	values []string
	index  int
}

func (s *rabbitRepairTestTokens) Next() (string, error) {
	if s.index >= len(s.values) {
		return "", errors.New("test token source exhausted")
	}
	value := s.values[s.index]
	s.index++
	return value, nil
}

type rabbitRepairTestBroker struct {
	highWater string
	pages     map[string]RecoveryPage[DispatchCandidate]
	events    *rabbitRepairTestEvents

	captureCalls int
	listAfter    []string
	listWater    []string
	listLimits   []int
	rearmCalls   []DispatchCandidate
	staleTasks   map[string]bool
	rearmErrTask string
	rearmResult  func(DispatchCandidate) (DispatchCandidate, bool, error)
}

func (s *rabbitRepairTestBroker) CaptureRepairHighWater(context.Context) (string, error) {
	s.events.add("capture-repair-high-water")
	s.captureCalls++
	return s.highWater, nil
}

func (s *rabbitRepairTestBroker) ListBrokerBackedCandidates(
	_ context.Context,
	highWater, after string,
	limit int,
) (RecoveryPage[DispatchCandidate], error) {
	s.events.add("list-repair:" + after)
	s.listAfter = append(s.listAfter, after)
	s.listWater = append(s.listWater, highWater)
	s.listLimits = append(s.listLimits, limit)
	page, ok := s.pages[after]
	if !ok {
		return RecoveryPage[DispatchCandidate]{}, fmt.Errorf("unexpected cursor %q", after)
	}
	return page, nil
}

func (s *rabbitRepairTestBroker) RearmAfterBrokerLoss(
	_ context.Context,
	original DispatchCandidate,
) (DispatchCandidate, bool, error) {
	s.events.add("rearm:" + original.Message.TaskID)
	s.rearmCalls = append(s.rearmCalls, original)
	if original.Message.TaskID == s.rearmErrTask {
		return DispatchCandidate{}, false, errors.New("injected rearm failure")
	}
	if s.staleTasks[original.Message.TaskID] {
		return DispatchCandidate{}, false, nil
	}
	if s.rearmResult != nil {
		return s.rearmResult(original)
	}
	updated := original
	updated.Message.DispatchToken.Generation++
	updated.Guard += ":rearmed"
	return updated, true, nil
}

type rabbitRepairTestJournal struct {
	mu     sync.Mutex
	record RecoveryOperationRecord
	found  bool
	events *rabbitRepairTestEvents

	beginCalls     int
	highWaterCalls int
	passCalls      int
	commitCalls    int
	completeCalls  int
}

type rabbitRepairTestStartSession struct{ journal *rabbitRepairTestJournal }

func (j *rabbitRepairTestJournal) Read(
	context.Context,
	string,
	string,
) (RecoveryOperationRecord, bool, error) {
	j.events.add("journal-read")
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.record, j.found, nil
}

func (j *rabbitRepairTestJournal) WithStartFence(
	ctx context.Context,
	_, _ string,
	fn func(OperationStartSession) error,
) error {
	j.events.add("start-fence-enter")
	err := fn(&rabbitRepairTestStartSession{journal: j})
	j.events.add("start-fence-exit")
	return err
}

func (s *rabbitRepairTestStartSession) Read(context.Context) (RecoveryOperationRecord, bool, error) {
	s.journal.events.add("start-journal-read")
	s.journal.mu.Lock()
	defer s.journal.mu.Unlock()
	return s.journal.record, s.journal.found, nil
}

func (s *rabbitRepairTestStartSession) BeginSpecial(
	_ context.Context,
	_ *RecoveryOperationRecord,
	proposal RecoveryOperationRecord,
) (RecoveryOperationRecord, error) {
	j := s.journal
	j.events.add("journal-begin")
	j.mu.Lock()
	defer j.mu.Unlock()
	j.beginCalls++
	if j.found && j.record.Phase != OperationCompleted {
		if j.record.OperationID != proposal.OperationID || j.record.Kind != proposal.Kind ||
			j.record.CurrentWriterFingerprint != proposal.CurrentWriterFingerprint {
			return RecoveryOperationRecord{}, errors.New("journal operation conflict")
		}
		return j.record, nil
	}
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	version := int64(1)
	if j.found {
		version = j.record.Version + 1
	}
	j.record = proposal
	j.record.Phase = OperationActive
	j.record.Version = version
	j.record.CreatedAt = now
	j.record.UpdatedAt = now
	j.found = true
	return j.record, nil
}

func (j *rabbitRepairTestJournal) update(
	event string,
	fn func(*RecoveryOperationRecord),
) (RecoveryOperationRecord, error) {
	j.events.add(event)
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.found {
		return RecoveryOperationRecord{}, errors.New("journal record missing")
	}
	fn(&j.record)
	j.record.Version++
	j.record.UpdatedAt = j.record.UpdatedAt.Add(time.Millisecond)
	return j.record, nil
}

func (j *rabbitRepairTestJournal) SetRepairHighWater(
	_ context.Context,
	_ RecoveryOperationRecord,
	highWater string,
) (RecoveryOperationRecord, error) {
	j.highWaterCalls++
	return j.update("journal-repair-high-water", func(record *RecoveryOperationRecord) {
		value := highWater
		record.RepairHighWater = &value
	})
}

func (j *rabbitRepairTestJournal) MarkRepairPassComplete(
	context.Context,
	RecoveryOperationRecord,
) (RecoveryOperationRecord, error) {
	j.passCalls++
	return j.update("journal-repair-pass", func(record *RecoveryOperationRecord) {
		record.RepairPassComplete = true
	})
}

func (j *rabbitRepairTestJournal) MarkForceDeletePassComplete(
	context.Context,
	RecoveryOperationRecord,
) (RecoveryOperationRecord, error) {
	return RecoveryOperationRecord{}, errors.New("unexpected force-delete journal call")
}

func (j *rabbitRepairTestJournal) CommitReady(
	context.Context,
	RecoveryOperationRecord,
) (RecoveryOperationRecord, error) {
	j.commitCalls++
	return j.update("journal-commit-ready", func(record *RecoveryOperationRecord) {
		record.Phase = OperationReadyCommitted
	})
}

func (j *rabbitRepairTestJournal) Complete(
	context.Context,
	RecoveryOperationRecord,
) (RecoveryOperationRecord, error) {
	j.completeCalls++
	return j.update("journal-complete", func(record *RecoveryOperationRecord) {
		record.Phase = OperationCompleted
	})
}

type rabbitRepairTestCoordinator struct {
	Coordinator
	events   *rabbitRepairTestEvents
	controls []RecoveryControlSnapshot
	inspect  int

	beginCalls      int
	setWaterCalls   int
	markPassCalls   int
	releaseCalls    int
	releaseBounded  bool
	lastOperationID string

	checkCalls         int
	renewLockCalls     int
	renewRecoveryCalls int
	checkErrAt         int
	renewLockErrAt     int
	renewRecoveryErrAt int
	checkErr           error
	renewLockErr       error
	renewRecoveryErr   error
}

func (c *rabbitRepairTestCoordinator) AcquireRecoveryLock(
	_ context.Context,
	_ string,
	owner string,
	_ time.Duration,
) (RecoveryLock, error) {
	c.events.add("redis-acquire-lock")
	return RecoveryLock{OwnerToken: owner}, nil
}

func (c *rabbitRepairTestCoordinator) CheckRecoveryLock(context.Context, string, RecoveryLock) error {
	c.events.add("redis-check-lock")
	c.checkCalls++
	if c.checkErrAt > 0 && c.checkCalls == c.checkErrAt {
		if c.checkErr != nil {
			return c.checkErr
		}
		return ErrRecoveryOwnerStale
	}
	return nil
}

func (c *rabbitRepairTestCoordinator) RenewRecoveryLock(context.Context, string, RecoveryLock, time.Duration) error {
	c.events.add("redis-renew-lock")
	c.renewLockCalls++
	if c.renewLockErrAt > 0 && c.renewLockCalls == c.renewLockErrAt {
		if c.renewLockErr != nil {
			return c.renewLockErr
		}
		return ErrRecoveryOwnerStale
	}
	return nil
}

func (c *rabbitRepairTestCoordinator) InspectRecoveryStart(
	context.Context,
	string,
	RecoveryLock,
) (RecoveryControlSnapshot, error) {
	c.events.add("redis-inspect-start")
	if len(c.controls) == 0 {
		return RecoveryControlSnapshot{}, errors.New("test control missing")
	}
	index := c.inspect
	if index >= len(c.controls) {
		index = len(c.controls) - 1
	}
	c.inspect++
	return c.controls[index], nil
}

func (c *rabbitRepairTestCoordinator) ReleaseRecoveryLock(
	ctx context.Context,
	_ string,
	_ RecoveryLock,
) error {
	c.events.add("redis-release-lock")
	c.releaseCalls++
	_, c.releaseBounded = ctx.Deadline()
	return nil
}

func (c *rabbitRepairTestCoordinator) BeginRabbitRepairWithLock(
	_ context.Context,
	_ string,
	writer, operationID string,
	lock RecoveryLock,
	_ time.Duration,
) (RecoveryFence, error) {
	c.events.add("redis-begin-repair")
	c.beginCalls++
	c.lastOperationID = operationID
	return RecoveryFence{
		ResourceFence: ResourceFence{Epoch: strings.Repeat("c", 32), WriterFingerprint: writer},
		OwnerToken:    lock.OwnerToken, Kind: RecoveryRabbitRepair, OperationID: operationID,
	}, nil
}

func (c *rabbitRepairTestCoordinator) RenewRecovery(
	context.Context,
	string,
	RecoveryFence,
	time.Duration,
) error {
	c.events.add("redis-renew-recovery")
	c.renewRecoveryCalls++
	if c.renewRecoveryErrAt > 0 && c.renewRecoveryCalls == c.renewRecoveryErrAt {
		if c.renewRecoveryErr != nil {
			return c.renewRecoveryErr
		}
		return ErrRecoveryOwnerStale
	}
	return nil
}

func (c *rabbitRepairTestCoordinator) SetRabbitRepairHighWater(
	context.Context,
	string,
	RecoveryFence,
	string,
) error {
	c.events.add("redis-repair-high-water")
	c.setWaterCalls++
	return nil
}

func (c *rabbitRepairTestCoordinator) MarkRabbitRepairPassComplete(
	context.Context,
	string,
	RecoveryFence,
) error {
	c.events.add("redis-repair-pass")
	c.markPassCalls++
	return nil
}

type rabbitRepairTestRecovery struct {
	events      *rabbitRepairTestEvents
	drainCalls  int
	runCalls    int
	finishCalls int
	runErr      error
}

func (r *rabbitRepairTestRecovery) DrainAttempts(context.Context, ResourceConfig) error {
	r.events.add("recovery-drain-attempts")
	r.drainCalls++
	return nil
}

func (r *rabbitRepairTestRecovery) RunRecovery(
	context.Context,
	ResourceConfig,
	RecoveryFence,
	RecoverySource,
) error {
	r.events.add("recovery-run-common")
	r.runCalls++
	return r.runErr
}

func (r *rabbitRepairTestRecovery) FinishRecovery(
	_ context.Context,
	_ ResourceConfig,
	fence RecoveryFence,
) (ResourceFence, error) {
	r.events.add("recovery-finish")
	r.finishCalls++
	return ResourceFence{Epoch: fence.Epoch, WriterFingerprint: fence.WriterFingerprint}, nil
}

type rabbitRepairTestRecoverySource struct{ RecoverySource }

func rabbitRepairTestConfig() ResourceConfig {
	return ResourceConfig{
		Key: "rag.index", ValidateTaskID: ValidateRAGIndexTaskID,
		LocalWorkers: 1, GlobalConcurrency: 1, PerUserBaseConcurrency: 1,
		PerUserBurstConcurrency: 1, BorrowEnabled: true,
		ReconcileInterval: 20 * time.Millisecond, ExpiredRunningSweepInterval: 20 * time.Millisecond,
		ReconcilePageSize: 2, ReservationTTL: 100 * time.Millisecond,
		ReservationHeartbeat: 10 * time.Millisecond, PrepareTimeout: 10 * time.Millisecond,
		ProvisionalTTL: 25 * time.Millisecond, ProcessingTurnTTL: 25 * time.Millisecond,
		RecoveryDrainTimeout: 200 * time.Millisecond, DispatchInterval: 20 * time.Millisecond,
		PublishAttemptTimeout: 5 * time.Millisecond,
	}
}

func rabbitRepairTestCandidate(taskID string, generation uint64) DispatchCandidate {
	return DispatchCandidate{
		Message: Message{
			Version: MessageVersion1, Resource: "rag.index", TenantID: "tenant-a",
			TaskType: "rag.index", TaskID: taskID,
			DispatchToken: DispatchToken{Resource: "rag.index", TaskID: taskID, Generation: generation},
		},
		Guard: "guard-" + taskID,
	}
}

func rabbitRepairTestReadyControl() RecoveryControlSnapshot {
	return RecoveryControlSnapshot{
		Present: true, State: ResourceReady, Epoch: strings.Repeat("d", 32),
		ProtocolVersion: ProtocolVersion, WriterFingerprint: rabbitRepairTestWriter,
		Kind: RecoveryNone,
	}
}

func rabbitRepairTestRecoveringControl(operationID, highWater string, complete bool) RecoveryControlSnapshot {
	return RecoveryControlSnapshot{
		Present: true, State: ResourceRecovering, Epoch: strings.Repeat("d", 32),
		ProtocolVersion: ProtocolVersion, WriterFingerprint: rabbitRepairTestWriter,
		Kind: RecoveryRabbitRepair, OperationID: operationID,
		Progress: &RecoveryProgress{
			Kind: RecoveryRabbitRepair, OperationID: operationID,
			RabbitRepair: &RabbitRepairProgress{
				RepairHighWater: highWater, RepairPassComplete: complete,
			},
		},
	}
}

func rabbitRepairTestRecord(phase OperationPhase, highWater *string, complete bool) RecoveryOperationRecord {
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	return RecoveryOperationRecord{
		Resource: "rag.index", OperationID: rabbitRepairTestOp,
		Kind: RecoveryRabbitRepair, Phase: phase,
		CurrentWriterFingerprint: rabbitRepairTestWriter,
		RepairHighWater:          highWater, RepairPassComplete: complete,
		Version: 3, CreatedAt: now, UpdatedAt: now,
	}
}

func newRabbitRepairTestSubject(
	t *testing.T,
	broker *rabbitRepairTestBroker,
	journal *rabbitRepairTestJournal,
	coordinator *rabbitRepairTestCoordinator,
	recovery *rabbitRepairTestRecovery,
) *RabbitDisasterRepair {
	t.Helper()
	repair, err := NewRabbitDisasterRepair(
		rabbitRepairTestConfig(), WriterIdentity{Fingerprint: rabbitRepairTestWriter},
		broker, rabbitRepairTestRecoverySource{}, journal, coordinator, recovery,
		RabbitDisasterRepairOptions{RecoveryLockTTL: time.Second, LockRenewInterval: 500 * time.Millisecond},
	)
	if err != nil {
		t.Fatalf("NewRabbitDisasterRepair() error = %v", err)
	}
	return repair
}

func TestRabbitDisasterRepairCheckUsesOneBoundedHighWaterSnapshot(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	highWater := "99"
	journal := &rabbitRepairTestJournal{
		record: rabbitRepairTestRecord(OperationActive, &highWater, false), found: true, events: events,
	}
	broker := &rabbitRepairTestBroker{
		highWater: "100", events: events,
		pages: map[string]RecoveryPage[DispatchCandidate]{
			"": {
				Items:      []DispatchCandidate{rabbitRepairTestCandidate("1", 1), rabbitRepairTestCandidate("2", 2)},
				NextCursor: "2",
			},
			"2": {NextCursor: "3"},
			"3": {Items: []DispatchCandidate{rabbitRepairTestCandidate("3", 3)}, Done: true},
		},
	}
	coordinator := &rabbitRepairTestCoordinator{events: events}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	report, err := repair.Check(context.Background())
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if report.CandidateCount != 3 || report.PagesScanned != 3 || !report.Operation.Present ||
		report.Operation.Kind != RecoveryRabbitRepair || report.Operation.Phase != OperationActive {
		t.Fatalf("Check() report = %+v", report)
	}
	if broker.captureCalls != 1 || len(broker.listWater) != 3 {
		t.Fatalf("capture/list calls = %d/%d", broker.captureCalls, len(broker.listWater))
	}
	for index := range broker.listWater {
		if broker.listWater[index] != "100" || broker.listLimits[index] != 2 {
			t.Fatalf("list call %d high-water/limit = %q/%d", index, broker.listWater[index], broker.listLimits[index])
		}
	}
	if journal.beginCalls != 0 || coordinator.beginCalls != 0 || len(broker.rearmCalls) != 0 || recovery.runCalls != 0 {
		t.Fatal("dry-run performed a mutation")
	}
}

func TestRabbitDisasterRepairApplyOrdersDrainJournalRedisCASAndFinish(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	candidate1 := rabbitRepairTestCandidate("1", 4)
	candidate2 := rabbitRepairTestCandidate("2", 8)
	broker := &rabbitRepairTestBroker{
		highWater: "2", events: events,
		pages: map[string]RecoveryPage[DispatchCandidate]{
			"":  {Items: []DispatchCandidate{candidate1}, NextCursor: "1"},
			"1": {Items: []DispatchCandidate{candidate2}, Done: true},
		},
	}
	journal := &rabbitRepairTestJournal{events: events}
	coordinator := &rabbitRepairTestCoordinator{events: events, controls: []RecoveryControlSnapshot{rabbitRepairTestReadyControl()}}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1, rabbitRepairTestOp}}
	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if !journal.found || journal.record.Phase != OperationCompleted || journal.record.RepairHighWater == nil ||
		*journal.record.RepairHighWater != "2" || !journal.record.RepairPassComplete {
		t.Fatalf("journal record = %+v", journal.record)
	}
	if len(broker.rearmCalls) != 2 || broker.rearmCalls[0].Guard != candidate1.Guard ||
		broker.rearmCalls[1].Guard != candidate2.Guard {
		t.Fatalf("CAS rearm originals = %+v", broker.rearmCalls)
	}
	if recovery.drainCalls != 1 || recovery.runCalls != 1 || recovery.finishCalls != 1 ||
		coordinator.beginCalls != 1 || coordinator.setWaterCalls != 1 || coordinator.markPassCalls != 1 {
		t.Fatalf("drain/run/finish/begin/water/pass = %d/%d/%d/%d/%d/%d",
			recovery.drainCalls, recovery.runCalls, recovery.finishCalls,
			coordinator.beginCalls, coordinator.setWaterCalls, coordinator.markPassCalls)
	}
	if !coordinator.releaseBounded {
		t.Fatal("raw recovery lock cleanup did not use a bounded context")
	}
	ordered := events.snapshot()
	rabbitRepairTestBefore(t, ordered, "redis-begin-repair", "start-fence-exit")
	rabbitRepairTestBefore(t, ordered, "start-fence-exit", "recovery-drain-attempts")
	rabbitRepairTestBefore(t, ordered, "recovery-drain-attempts", "capture-repair-high-water")
	rabbitRepairTestBefore(t, ordered, "journal-repair-high-water", "redis-repair-high-water")
	rabbitRepairTestBefore(t, ordered, "redis-repair-high-water", "rearm:1")
	rabbitRepairTestBefore(t, ordered, "rearm:2", "journal-repair-pass")
	rabbitRepairTestBefore(t, ordered, "journal-repair-pass", "redis-repair-pass")
	rabbitRepairTestBefore(t, ordered, "redis-repair-pass", "recovery-run-common")
	rabbitRepairTestBefore(t, ordered, "journal-commit-ready", "recovery-finish")
	rabbitRepairTestBefore(t, ordered, "recovery-finish", "journal-complete")
}

func TestRabbitDisasterRepairApplyRehydratesMissingRedisFromActiveJournal(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	highWater := "2"
	journal := &rabbitRepairTestJournal{
		record: rabbitRepairTestRecord(OperationActive, &highWater, false), found: true, events: events,
	}
	broker := &rabbitRepairTestBroker{
		highWater: "new-water-must-not-be-used", events: events,
		pages: map[string]RecoveryPage[DispatchCandidate]{
			"": {
				Items: []DispatchCandidate{rabbitRepairTestCandidate("1", 4), rabbitRepairTestCandidate("2", 8)},
				Done:  true,
			},
		},
		staleTasks: map[string]bool{"1": true},
	}
	coordinator := &rabbitRepairTestCoordinator{
		events: events, controls: []RecoveryControlSnapshot{{}},
	}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner2}}
	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if err != nil {
		t.Fatalf("Apply(resume) error = %v", err)
	}
	if coordinator.lastOperationID != rabbitRepairTestOp || broker.captureCalls != 0 ||
		journal.highWaterCalls != 0 || len(broker.rearmCalls) != 2 || broker.listAfter[0] != "" {
		t.Fatalf("resume op/capture/highwater/rearm/cursor = %q/%d/%d/%d/%v",
			coordinator.lastOperationID, broker.captureCalls, journal.highWaterCalls,
			len(broker.rearmCalls), broker.listAfter)
	}
	if journal.record.Phase != OperationCompleted || !journal.record.RepairPassComplete {
		t.Fatalf("resumed journal record = %+v", journal.record)
	}
}

func TestRabbitDisasterRepairApplyRequiresAttestationBeforeMutation(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	broker := &rabbitRepairTestBroker{events: events}
	journal := &rabbitRepairTestJournal{events: events}
	coordinator := &rabbitRepairTestCoordinator{events: events}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	for _, attestation := range []RabbitRepairAttestation{
		{}, {OldBrokerIsolated: true}, {PublishersPaused: true},
	} {
		if err := repair.Apply(context.Background(), attestation); !errors.Is(err, ErrOperatorConfirmation) {
			t.Fatalf("Apply(%+v) error = %v", attestation, err)
		}
	}
	if values := events.snapshot(); len(values) != 0 {
		t.Fatalf("missing attestation caused side effects: %v", values)
	}
}

func TestRabbitDisasterRepairReadyCommittedTerminalOnlyCompletesJournal(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	highWater := "2"
	journal := &rabbitRepairTestJournal{
		record: rabbitRepairTestRecord(OperationReadyCommitted, &highWater, true), found: true, events: events,
	}
	control := rabbitRepairTestReadyControl()
	control.LastCompletedOperationID = rabbitRepairTestOp
	broker := &rabbitRepairTestBroker{events: events}
	coordinator := &rabbitRepairTestCoordinator{events: events, controls: []RecoveryControlSnapshot{control}}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1}}
	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if err != nil {
		t.Fatalf("Apply(terminal) error = %v", err)
	}
	if journal.record.Phase != OperationCompleted || journal.completeCalls != 1 || journal.beginCalls != 0 ||
		coordinator.beginCalls != 0 || broker.captureCalls != 0 || len(broker.rearmCalls) != 0 ||
		recovery.drainCalls != 0 || recovery.runCalls != 0 || recovery.finishCalls != 0 {
		t.Fatalf("terminal calls record=%+v begin=%d redis-begin=%d capture=%d rearm=%d recovery=%d/%d/%d",
			journal.record, journal.beginCalls, coordinator.beginCalls, broker.captureCalls,
			len(broker.rearmCalls), recovery.drainCalls, recovery.runCalls, recovery.finishCalls)
	}
}

func TestRabbitDisasterRepairRejectsConflictingUnfinishedOperationBeforeJournalMutation(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	now := time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)
	notBefore := now
	journal := &rabbitRepairTestJournal{
		record: RecoveryOperationRecord{
			Resource: "rag.index", OperationID: rabbitRepairTestOp,
			Kind: RecoveryForceRebuild, Phase: OperationActive,
			CurrentWriterFingerprint: rabbitRepairTestWriter, ForceNotBefore: &notBefore,
			Version: 1, CreatedAt: now, UpdatedAt: now,
		},
		found: true, events: events,
	}
	broker := &rabbitRepairTestBroker{events: events}
	coordinator := &rabbitRepairTestCoordinator{events: events, controls: []RecoveryControlSnapshot{rabbitRepairTestReadyControl()}}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1}}
	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if !errors.Is(err, ErrResourceNotReady) || !errors.Is(err, ErrRecoveryOperatorRequired) {
		t.Fatalf("Apply(conflict) error = %v", err)
	}
	if journal.beginCalls != 0 || coordinator.beginCalls != 0 || broker.captureCalls != 0 || len(broker.rearmCalls) != 0 {
		t.Fatal("conflicting operation caused journal/Redis/business mutation")
	}
	if !coordinator.releaseBounded {
		t.Fatal("conflict cleanup did not use a bounded lock-release context")
	}
}

func TestRabbitDisasterRepairResumesHalfPageFailureFromJournalHighWater(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	broker := &rabbitRepairTestBroker{
		highWater: "2", events: events, rearmErrTask: "2",
		pages: map[string]RecoveryPage[DispatchCandidate]{
			"": {
				Items: []DispatchCandidate{
					rabbitRepairTestCandidate("1", 3),
					rabbitRepairTestCandidate("2", 7),
				},
				Done: true,
			},
		},
	}
	journal := &rabbitRepairTestJournal{events: events}
	coordinator := &rabbitRepairTestCoordinator{
		events: events, controls: []RecoveryControlSnapshot{rabbitRepairTestReadyControl()},
	}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	repair.tokens = &rabbitRepairTestTokens{values: []string{
		rabbitRepairTestOwner1, rabbitRepairTestOp, rabbitRepairTestOwner2,
	}}
	attestation := RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true}

	err := repair.Apply(context.Background(), attestation)
	if err == nil || !strings.Contains(err.Error(), "injected rearm failure") {
		t.Fatalf("Apply(first half-page) error = %v", err)
	}
	if journal.record.Phase != OperationActive || journal.record.RepairHighWater == nil ||
		*journal.record.RepairHighWater != "2" || journal.record.RepairPassComplete ||
		journal.passCalls != 0 || journal.commitCalls != 0 || recovery.runCalls != 0 {
		t.Fatalf("half-page journal/calls = %+v pass=%d commit=%d recovery=%d",
			journal.record, journal.passCalls, journal.commitCalls, recovery.runCalls)
	}

	// Redis control/progress is now gone. The first CAS is stale on retry, as it
	// would be against the original opaque Guard in the authoritative store.
	coordinator.controls = []RecoveryControlSnapshot{{}}
	coordinator.inspect = 0
	broker.rearmErrTask = ""
	broker.staleTasks = map[string]bool{"1": true}
	if err := repair.Apply(context.Background(), attestation); err != nil {
		t.Fatalf("Apply(resume after Redis loss) error = %v", err)
	}
	if journal.record.Phase != OperationCompleted || journal.highWaterCalls != 1 ||
		journal.passCalls != 1 || broker.captureCalls != 1 || coordinator.beginCalls != 2 ||
		coordinator.lastOperationID != rabbitRepairTestOp {
		t.Fatalf("resume record/calls = %+v high-water=%d pass=%d capture=%d begin=%d op=%q",
			journal.record, journal.highWaterCalls, journal.passCalls, broker.captureCalls,
			coordinator.beginCalls, coordinator.lastOperationID)
	}
	if len(broker.listAfter) != 2 || broker.listAfter[0] != "" || broker.listAfter[1] != "" ||
		len(broker.rearmCalls) != 4 {
		t.Fatalf("resume cursors/rearms = %v/%d", broker.listAfter, len(broker.rearmCalls))
	}
}

func TestRabbitDisasterRepairRawLockFencesJournalCAS(t *testing.T) {
	attestation := RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true}

	t.Run("renew lost before CAS leaves no journal", func(t *testing.T) {
		events := &rabbitRepairTestEvents{}
		journal := &rabbitRepairTestJournal{events: events}
		coordinator := &rabbitRepairTestCoordinator{
			events: events, controls: []RecoveryControlSnapshot{rabbitRepairTestReadyControl()},
			renewLockErrAt: 2,
		}
		repair := newRabbitRepairTestSubject(t, &rabbitRepairTestBroker{events: events}, journal,
			coordinator, &rabbitRepairTestRecovery{events: events})
		repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1, rabbitRepairTestOp}}

		err := repair.Apply(context.Background(), attestation)
		if !errors.Is(err, ErrRecoveryOwnerStale) {
			t.Fatalf("Apply(pre-CAS lock loss) error = %v", err)
		}
		if journal.found || journal.beginCalls != 0 || coordinator.beginCalls != 0 {
			t.Fatalf("pre-CAS lock loss mutated state: found=%v journal-begin=%d redis-begin=%d",
				journal.found, journal.beginCalls, coordinator.beginCalls)
		}
	})

	t.Run("check lost after CAS leaves resumable ACTIVE only", func(t *testing.T) {
		events := &rabbitRepairTestEvents{}
		journal := &rabbitRepairTestJournal{events: events}
		coordinator := &rabbitRepairTestCoordinator{
			events: events, controls: []RecoveryControlSnapshot{rabbitRepairTestReadyControl()},
			checkErrAt: 3,
		}
		repair := newRabbitRepairTestSubject(t, &rabbitRepairTestBroker{events: events}, journal,
			coordinator, &rabbitRepairTestRecovery{events: events})
		repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1, rabbitRepairTestOp}}

		err := repair.Apply(context.Background(), attestation)
		if !errors.Is(err, ErrRecoveryOwnerStale) {
			t.Fatalf("Apply(post-CAS lock loss) error = %v", err)
		}
		if !journal.found || journal.record.Phase != OperationActive || journal.beginCalls != 1 ||
			coordinator.beginCalls != 0 {
			t.Fatalf("post-CAS state = found=%v record=%+v journal-begin=%d redis-begin=%d",
				journal.found, journal.record, journal.beginCalls, coordinator.beginCalls)
		}
	})
}

func TestRabbitDisasterRepairRejectsControlTOCTOUAfterJournalCAS(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	otherOperation := strings.Repeat("e", 32)
	journal := &rabbitRepairTestJournal{events: events}
	coordinator := &rabbitRepairTestCoordinator{
		events: events,
		controls: []RecoveryControlSnapshot{
			rabbitRepairTestReadyControl(),
			rabbitRepairTestRecoveringControl(otherOperation, "", false),
		},
	}
	broker := &rabbitRepairTestBroker{events: events}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1, rabbitRepairTestOp}}

	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if !errors.Is(err, ErrRecoveryOperatorRequired) || !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("Apply(control TOCTOU) error = %v", err)
	}
	if journal.record.Phase != OperationActive || journal.beginCalls != 1 || coordinator.beginCalls != 0 ||
		broker.captureCalls != 0 || len(broker.rearmCalls) != 0 || recovery.drainCalls != 0 {
		t.Fatalf("TOCTOU mutations record=%+v journal-begin=%d redis-begin=%d capture=%d rearm=%d drain=%d",
			journal.record, journal.beginCalls, coordinator.beginCalls, broker.captureCalls,
			len(broker.rearmCalls), recovery.drainCalls)
	}
}

func TestRabbitDisasterRepairFencedRenewalProtectsReadyTransitions(t *testing.T) {
	tests := []struct {
		name              string
		failAt            int
		wantPhase         OperationPhase
		wantCommitCalls   int
		wantFinishCalls   int
		wantCompleteCalls int
	}{
		{
			name: "common rebuild to journal commit", failAt: 5,
			wantPhase: OperationActive,
		},
		{
			name: "journal commit to Redis finish", failAt: 6,
			wantPhase: OperationReadyCommitted, wantCommitCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := &rabbitRepairTestEvents{}
			broker := &rabbitRepairTestBroker{
				highWater: "1", events: events,
				pages: map[string]RecoveryPage[DispatchCandidate]{
					"": {Items: []DispatchCandidate{rabbitRepairTestCandidate("1", 4)}, Done: true},
				},
			}
			journal := &rabbitRepairTestJournal{events: events}
			coordinator := &rabbitRepairTestCoordinator{
				events: events, controls: []RecoveryControlSnapshot{rabbitRepairTestReadyControl()},
				renewRecoveryErrAt: test.failAt,
			}
			recovery := &rabbitRepairTestRecovery{events: events}
			repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
			repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1, rabbitRepairTestOp}}

			err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
			if !errors.Is(err, ErrRecoveryOwnerStale) {
				t.Fatalf("Apply(renew failure %d) error = %v", test.failAt, err)
			}
			if journal.record.Phase != test.wantPhase || journal.commitCalls != test.wantCommitCalls ||
				recovery.finishCalls != test.wantFinishCalls || journal.completeCalls != test.wantCompleteCalls {
				t.Fatalf("renew failure %d state=%+v commit=%d finish=%d complete=%d",
					test.failAt, journal.record, journal.commitCalls, recovery.finishCalls, journal.completeCalls)
			}
		})
	}
}

func TestRabbitDisasterRepairOwnerLossAfterPageDoesNotMarkPass(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	broker := &rabbitRepairTestBroker{
		highWater: "1", events: events,
		pages: map[string]RecoveryPage[DispatchCandidate]{
			"": {Items: []DispatchCandidate{rabbitRepairTestCandidate("1", 2)}, Done: true},
		},
	}
	journal := &rabbitRepairTestJournal{events: events}
	coordinator := &rabbitRepairTestCoordinator{
		events: events, controls: []RecoveryControlSnapshot{rabbitRepairTestReadyControl()},
		renewRecoveryErrAt: 4,
	}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1, rabbitRepairTestOp}}

	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if !errors.Is(err, ErrRecoveryOwnerStale) {
		t.Fatalf("Apply(page fence loss) error = %v", err)
	}
	if len(broker.rearmCalls) != 1 || journal.passCalls != 0 || journal.record.RepairPassComplete ||
		coordinator.markPassCalls != 0 || recovery.runCalls != 0 || journal.commitCalls != 0 ||
		recovery.finishCalls != 0 || journal.completeCalls != 0 {
		t.Fatalf("page fence loss rearm=%d record=%+v journal-pass=%d redis-pass=%d run=%d commit=%d finish=%d complete=%d",
			len(broker.rearmCalls), journal.record, journal.passCalls, coordinator.markPassCalls,
			recovery.runCalls, journal.commitCalls, recovery.finishCalls, journal.completeCalls)
	}
}

func TestRabbitDisasterRepairRejectsChangedCASIdentity(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	broker := &rabbitRepairTestBroker{
		highWater: "1", events: events,
		pages: map[string]RecoveryPage[DispatchCandidate]{
			"": {Items: []DispatchCandidate{rabbitRepairTestCandidate("1", 2)}, Done: true},
		},
		rearmResult: func(original DispatchCandidate) (DispatchCandidate, bool, error) {
			updated := original
			updated.Message.TenantID = "tenant-b"
			updated.Message.DispatchToken.Generation++
			return updated, true, nil
		},
	}
	journal := &rabbitRepairTestJournal{events: events}
	coordinator := &rabbitRepairTestCoordinator{
		events: events, controls: []RecoveryControlSnapshot{rabbitRepairTestReadyControl()},
	}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1, rabbitRepairTestOp}}

	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if !errors.Is(err, ErrInvalidRecoveryState) {
		t.Fatalf("Apply(changed CAS identity) error = %v", err)
	}
	if journal.passCalls != 0 || recovery.runCalls != 0 || recovery.finishCalls != 0 {
		t.Fatalf("invalid CAS advanced repair: pass=%d run=%d finish=%d",
			journal.passCalls, recovery.runCalls, recovery.finishCalls)
	}
}

func TestRabbitDisasterRepairRejectsRedisProgressAheadOfJournal(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	highWater := "2"
	journal := &rabbitRepairTestJournal{
		record: rabbitRepairTestRecord(OperationActive, &highWater, false), found: true, events: events,
	}
	coordinator := &rabbitRepairTestCoordinator{
		events:   events,
		controls: []RecoveryControlSnapshot{rabbitRepairTestRecoveringControl(rabbitRepairTestOp, "3", false)},
	}
	broker := &rabbitRepairTestBroker{events: events}
	recovery := &rabbitRepairTestRecovery{events: events}
	repair := newRabbitRepairTestSubject(t, broker, journal, coordinator, recovery)
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1}}

	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("Apply(progress mismatch) error = %v", err)
	}
	if journal.beginCalls != 0 || coordinator.beginCalls != 0 || broker.captureCalls != 0 || recovery.drainCalls != 0 {
		t.Fatalf("progress mismatch mutated state: journal-begin=%d redis-begin=%d capture=%d drain=%d",
			journal.beginCalls, coordinator.beginCalls, broker.captureCalls, recovery.drainCalls)
	}
}

func TestRabbitDisasterRepairRejectsMalformedMissingControlBeforeJournalMutation(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	journal := &rabbitRepairTestJournal{events: events}
	coordinator := &rabbitRepairTestCoordinator{
		events:   events,
		controls: []RecoveryControlSnapshot{{Epoch: strings.Repeat("d", 32)}},
	}
	repair := newRabbitRepairTestSubject(t, &rabbitRepairTestBroker{events: events}, journal,
		coordinator, &rabbitRepairTestRecovery{events: events})
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1, rabbitRepairTestOp}}

	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if !errors.Is(err, ErrCoordinationCorrupt) || !errors.Is(err, ErrInvalidRecoveryState) {
		t.Fatalf("Apply(malformed missing control) error = %v", err)
	}
	if journal.beginCalls != 0 || coordinator.beginCalls != 0 {
		t.Fatalf("malformed control mutated journal/control: %d/%d", journal.beginCalls, coordinator.beginCalls)
	}
}

func TestRabbitDisasterRepairRejectsOperationIDCollisionBeforeJournalMutation(t *testing.T) {
	events := &rabbitRepairTestEvents{}
	journal := &rabbitRepairTestJournal{events: events}
	control := rabbitRepairTestReadyControl()
	control.LastCompletedOperationID = rabbitRepairTestOp
	coordinator := &rabbitRepairTestCoordinator{events: events, controls: []RecoveryControlSnapshot{control}}
	repair := newRabbitRepairTestSubject(t, &rabbitRepairTestBroker{events: events}, journal,
		coordinator, &rabbitRepairTestRecovery{events: events})
	repair.tokens = &rabbitRepairTestTokens{values: []string{rabbitRepairTestOwner1, rabbitRepairTestOp}}

	err := repair.Apply(context.Background(), RabbitRepairAttestation{OldBrokerIsolated: true, PublishersPaused: true})
	if !errors.Is(err, ErrCoordinationCorrupt) {
		t.Fatalf("Apply(operation ID collision) error = %v", err)
	}
	if journal.beginCalls != 0 || coordinator.beginCalls != 0 {
		t.Fatalf("operation ID collision mutated journal/control: %d/%d", journal.beginCalls, coordinator.beginCalls)
	}
}
