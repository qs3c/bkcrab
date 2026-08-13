package fairqueue

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func recoveryTestConfig() ResourceConfig {
	return ResourceConfig{
		Key:                         "rag.index",
		ValidateTaskID:              ValidateRAGIndexTaskID,
		LocalWorkers:                1,
		GlobalConcurrency:           4,
		PerUserBaseConcurrency:      1,
		PerUserBurstConcurrency:     4,
		BorrowEnabled:               true,
		ReconcileInterval:           20 * time.Millisecond,
		ExpiredRunningSweepInterval: 20 * time.Millisecond,
		ReconcilePageSize:           2,
		ReservationTTL:              time.Second,
		ReservationHeartbeat:        100 * time.Millisecond,
		PrepareTimeout:              10 * time.Millisecond,
		ProvisionalTTL:              40 * time.Millisecond,
		ProcessingTurnTTL:           40 * time.Millisecond,
		RecoveryDrainTimeout:        250 * time.Millisecond,
		DispatchInterval:            20 * time.Millisecond,
		PublishAttemptTimeout:       15 * time.Millisecond,
	}
}

func recoveryTestWriter() string { return strings.Repeat("a", 64) }
func recoveryTestEpoch() string  { return strings.Repeat("b", 32) }
func recoveryTestOwner() string  { return strings.Repeat("c", 32) }

type recoveryStaticTokenSource struct{ token string }

func (s recoveryStaticTokenSource) Next() (string, error) { return s.token, nil }

func recoveryTestFence() RecoveryFence {
	return RecoveryFence{
		ResourceFence: ResourceFence{Epoch: recoveryTestEpoch(), WriterFingerprint: recoveryTestWriter()},
		OwnerToken:    recoveryTestOwner(),
		Kind:          RecoveryNormal,
	}
}

type recoveryTestRuntime struct {
	mu         sync.Mutex
	closed     int
	drained    int
	opened     int
	openFence  ResourceFence
	drainBlock <-chan struct{}
	openErr    error
}

func (r *recoveryTestRuntime) CloseResource(string) error {
	r.mu.Lock()
	r.closed++
	r.mu.Unlock()
	return nil
}

func (r *recoveryTestRuntime) WaitForAttemptDrain(ctx context.Context, _ string) error {
	r.mu.Lock()
	r.drained++
	block := r.drainBlock
	r.mu.Unlock()
	if block == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-block:
		return nil
	}
}

func (r *recoveryTestRuntime) OpenResource(_ string, fence ResourceFence) error {
	r.mu.Lock()
	r.opened++
	r.openFence = fence
	r.mu.Unlock()
	return r.openErr
}

type recoveryTestRabbit struct {
	fakeRabbitClient
	mu         sync.Mutex
	topology   []string
	depths     map[string]int64
	probeErr   error
	probe      RabbitResourceProbe
	probeCalls int
}

func (r *recoveryTestRabbit) ProbeResourceTopology(_ context.Context, resource string) (RabbitResourceProbe, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probeCalls++
	if resource == "" {
		return RabbitResourceProbe{}, errors.New("empty resource")
	}
	if r.probeErr != nil {
		return RabbitResourceProbe{}, r.probeErr
	}
	if r.probe != (RabbitResourceProbe{}) {
		return r.probe, nil
	}
	return RabbitResourceProbe{Resource: resource}, nil
}

func (r *recoveryTestRabbit) EnsureTenantTopology(_ context.Context, _, tenant string) error {
	r.mu.Lock()
	r.topology = append(r.topology, tenant)
	r.mu.Unlock()
	return nil
}

func (r *recoveryTestRabbit) ReadyDepth(_ context.Context, _, tenant string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.depths[tenant], nil
}

type recoveryPassUpdate struct {
	pass  RecoveryPass
	cycle uint64
	diff  int64
}

type recoveryTestCoordinator struct {
	fakeCoordinator
	mu sync.Mutex

	fence        RecoveryFence
	snapshot     RecoveryControlSnapshot
	ready        ResourceFence
	failRenew    bool
	renewCount   int
	observeCount int

	cleanup RecoveryCleanupResult
	owned   RecoveryPage[RedisKeyRef]

	known      []string
	active     []string
	stable     map[string]ReservationRef
	passes     []recoveryPassUpdate
	reset      int
	finish     int
	events     *[]string
	observeErr error
}

func newRecoveryTestCoordinator(fence RecoveryFence) *recoveryTestCoordinator {
	progress := &RecoveryProgress{Kind: fence.Kind, OperationID: fence.OperationID}
	return &recoveryTestCoordinator{
		fence: fence,
		snapshot: RecoveryControlSnapshot{
			Present: true, State: ResourceRecovering, Epoch: fence.Epoch,
			ProtocolVersion: ProtocolVersion, WriterFingerprint: fence.WriterFingerprint,
			Kind: fence.Kind, OperationID: fence.OperationID, Progress: progress,
		},
		ready:   fence.ResourceFence,
		owned:   RecoveryPage[RedisKeyRef]{Done: true},
		stable:  make(map[string]ReservationRef),
		cleanup: RecoveryCleanupResult{},
	}
}

func (c *recoveryTestCoordinator) RenewRecovery(context.Context, string, RecoveryFence, time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.renewCount++
	if c.failRenew {
		return ErrRecoveryOwnerStale
	}
	return nil
}

func (c *recoveryTestCoordinator) recordEvent(event string) {
	if c.events != nil {
		*c.events = append(*c.events, event)
	}
}

func (c *recoveryTestCoordinator) AcquireRecoveryLock(_ context.Context, _ string, owner string, _ time.Duration) (RecoveryLock, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordEvent("redis:acquire")
	return RecoveryLock{OwnerToken: owner}, nil
}

func (c *recoveryTestCoordinator) CheckRecoveryLock(context.Context, string, RecoveryLock) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordEvent("redis:check-raw")
	return nil
}

func (c *recoveryTestCoordinator) RenewRecoveryLock(context.Context, string, RecoveryLock, time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordEvent("redis:renew-raw")
	return nil
}

func (c *recoveryTestCoordinator) ReleaseRecoveryLock(context.Context, string, RecoveryLock) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordEvent("redis:release-raw")
	return nil
}

func (c *recoveryTestCoordinator) BeginRecoveryWithLock(_ context.Context, _ string, _ string, lock RecoveryLock, _ time.Duration) (RecoveryFence, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordEvent("redis:begin")
	c.fence.OwnerToken = lock.OwnerToken
	c.snapshot = RecoveryControlSnapshot{
		Present: true, State: ResourceRecovering, Epoch: c.fence.Epoch,
		ProtocolVersion: ProtocolVersion, WriterFingerprint: c.fence.WriterFingerprint,
		Kind: RecoveryNormal, Progress: &RecoveryProgress{Kind: RecoveryNormal},
	}
	return c.fence, nil
}

func (c *recoveryTestCoordinator) InspectRecoveryStart(context.Context, string, RecoveryLock) (RecoveryControlSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordEvent("redis:inspect")
	copy := c.snapshot
	if c.snapshot.Progress != nil {
		progress := *c.snapshot.Progress
		copy.Progress = &progress
	}
	return copy, nil
}

func (c *recoveryTestCoordinator) RecoveryReapExpired(context.Context, string, RecoveryFence, int) (RecoveryCleanupResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cleanup, nil
}

func (c *recoveryTestCoordinator) SetRecoveryHighWater(_ context.Context, _ string, _ RecoveryFence, highWater string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot.Progress.HighWater = highWater
	return nil
}

func (c *recoveryTestCoordinator) ResetResource(context.Context, string, RecoveryFence) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reset++
	c.stable = make(map[string]ReservationRef)
	return nil
}

func (c *recoveryTestCoordinator) ListOwnedResourceKeys(context.Context, string, RecoveryFence, string, int) (RecoveryPage[RedisKeyRef], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.owned, nil
}

func (c *recoveryTestCoordinator) DeleteOwnedResourceKeys(context.Context, string, RecoveryFence, []RedisKeyRef) error {
	return nil
}

func (c *recoveryTestCoordinator) RestoreKnownTenant(_ context.Context, _ string, _ RecoveryFence, tenant string) error {
	c.mu.Lock()
	c.known = append(c.known, tenant)
	c.mu.Unlock()
	return nil
}

func (c *recoveryTestCoordinator) RestoreActiveTenant(_ context.Context, _ string, _ RecoveryFence, tenant string) error {
	c.mu.Lock()
	c.active = append(c.active, tenant)
	c.mu.Unlock()
	return nil
}

func (c *recoveryTestCoordinator) RestoreInflight(_ context.Context, _ string, _ RecoveryFence, tenant, token string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stable[token] = ReservationRef{TenantID: tenant, StableToken: token, ExpiresAtUnixMS: ttl.Milliseconds()}
	return nil
}

func (c *recoveryTestCoordinator) ListRecoveryStableInflight(_ context.Context, _ string, _ RecoveryFence, after string, limit int) (RecoveryPage[ReservationRef], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	items := make([]ReservationRef, 0, len(c.stable))
	for _, item := range c.stable {
		if item.StableToken > after {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StableToken < items[j].StableToken })
	if len(items) > limit {
		page := RecoveryPage[ReservationRef]{Items: items[:limit], NextCursor: items[limit-1].StableToken}
		return page, nil
	}
	return RecoveryPage[ReservationRef]{Items: items, Done: true}, nil
}

func (c *recoveryTestCoordinator) DeleteRecoveryStableInflight(_ context.Context, _ string, _ RecoveryFence, ref ReservationRef) error {
	c.mu.Lock()
	delete(c.stable, ref.StableToken)
	c.mu.Unlock()
	return nil
}

func (c *recoveryTestCoordinator) MarkRecoveryPass(_ context.Context, _ string, _ RecoveryFence, pass RecoveryPass, cycle uint64, _ bool, diff int64) error {
	c.mu.Lock()
	c.passes = append(c.passes, recoveryPassUpdate{pass: pass, cycle: cycle, diff: diff})
	c.mu.Unlock()
	return nil
}

func (c *recoveryTestCoordinator) FinishRecovery(context.Context, string, RecoveryFence) error {
	c.mu.Lock()
	c.finish++
	c.mu.Unlock()
	return nil
}

func (c *recoveryTestCoordinator) ObserveReadyFence(context.Context, string, string) (ResourceFence, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observeCount++
	if c.observeErr != nil {
		return ResourceFence{}, c.observeErr
	}
	return c.ready, nil
}

func (c *recoveryTestCoordinator) CheckReadyFence(context.Context, string, ResourceFence) error {
	return nil
}

func (c *recoveryTestCoordinator) EnsureKnownTenant(_ context.Context, _ string, _ ResourceFence, tenant string) error {
	c.mu.Lock()
	c.known = append(c.known, tenant)
	c.mu.Unlock()
	return nil
}

func (c *recoveryTestCoordinator) EnsureActive(_ context.Context, _ string, _ ResourceFence, tenant string) error {
	c.mu.Lock()
	c.active = append(c.active, tenant)
	c.mu.Unlock()
	return nil
}

func (c *recoveryTestCoordinator) ListReadyStableInflight(_ context.Context, _ string, _ ResourceFence, after string, limit int) (RecoveryPage[ReservationRef], error) {
	return c.ListRecoveryStableInflight(context.Background(), "", RecoveryFence{}, after, limit)
}

func (c *recoveryTestCoordinator) EnsureReadyStableInflight(_ context.Context, _ string, _ ResourceFence, tenant, token string, ttl time.Duration) error {
	c.mu.Lock()
	c.stable[token] = ReservationRef{TenantID: tenant, StableToken: token, ExpiresAtUnixMS: ttl.Milliseconds()}
	c.mu.Unlock()
	return nil
}

func (c *recoveryTestCoordinator) Release(_ context.Context, _ string, _ ResourceFence, _ string, token string) error {
	c.mu.Lock()
	delete(c.stable, token)
	c.mu.Unlock()
	return nil
}

func (c *recoveryTestCoordinator) ReapExpiredTurnsAndProvisionals(context.Context, string, ResourceFence, int) (RecoveryCleanupResult, error) {
	return RecoveryCleanupResult{}, nil
}

type recoveryTestSource struct {
	mu sync.Mutex

	highWater  string
	captureErr error
	captures   int
	known      []TenantRef
	dispatch   []DispatchedRef
	running    []RunningLease
	seenHigh   []string
	afterPage  func()
}

func (s *recoveryTestSource) CaptureHighWater(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captures++
	if s.captureErr != nil {
		return "", s.captureErr
	}
	return s.highWater, nil
}

func pageSlice[T any](items []T, after string, limit int) RecoveryPage[T] {
	start := 0
	if after != "" {
		for index := range items {
			if recoveryTestCursor(items[index]) == after {
				start = index + 1
				break
			}
		}
	}
	end := min(len(items), start+limit)
	page := RecoveryPage[T]{Items: append([]T(nil), items[start:end]...), Done: end == len(items)}
	if !page.Done {
		page.NextCursor = recoveryTestCursor(items[end-1])
	}
	return page
}

func recoveryTestCursor(value any) string {
	switch item := value.(type) {
	case TenantRef:
		return item.TenantID
	case DispatchedRef:
		return item.Token.TaskID
	case RunningLease:
		return item.TaskID
	default:
		return "cursor"
	}
}

func (s *recoveryTestSource) noteHighWater(highWater string) {
	s.seenHigh = append(s.seenHigh, highWater)
}

func (s *recoveryTestSource) ListKnownTenants(_ context.Context, highWater, after string, limit int) (RecoveryPage[TenantRef], error) {
	s.mu.Lock()
	s.noteHighWater(highWater)
	page := pageSlice(s.known, after, limit)
	hook := s.afterPage
	s.afterPage = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return page, nil
}

func (s *recoveryTestSource) ListDispatched(_ context.Context, highWater, after string, limit int) (RecoveryPage[DispatchedRef], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteHighWater(highWater)
	return pageSlice(s.dispatch, after, limit), nil
}

func (s *recoveryTestSource) ListValidRunning(_ context.Context, highWater, after string, limit int) (RecoveryPage[RunningLease], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noteHighWater(highWater)
	return pageSlice(s.running, after, limit), nil
}

func newRecoveryTestRunner(t *testing.T, coordinator Coordinator, rabbit RabbitClient, runtime RecoveryRuntime) *RecoveryCoordinator {
	t.Helper()
	runner, err := NewRecoveryCoordinator(coordinator, rabbit, runtime, RecoveryOptions{
		LockTTL: 200 * time.Millisecond, LockRenewInterval: 50 * time.Millisecond,
		CleanupInterval: time.Millisecond, OperationTimeout: 100 * time.Millisecond,
		BackoffInitial: time.Millisecond, BackoffMax: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRecoveryCoordinator() error = %v", err)
	}
	return runner
}

func TestRecoveryOptionsZeroDefaultsAndRejectUnsafeRenewal(t *testing.T) {
	normalized, err := (RecoveryOptions{}).withDefaults()
	if err != nil {
		t.Fatalf("withDefaults() error = %v", err)
	}
	if normalized.LockTTL <= 0 || normalized.LockRenewInterval <= 0 ||
		normalized.LockRenewInterval >= normalized.LockTTL {
		t.Fatalf("unsafe defaults: %+v", normalized)
	}
	_, err = (RecoveryOptions{LockTTL: time.Second, LockRenewInterval: time.Second}).withDefaults()
	if err == nil {
		t.Fatal("equal renewal interval and TTL was accepted")
	}
}

func TestRecoveryDrainAttemptsClosesAndWaitsFullPublishWindow(t *testing.T) {
	config := recoveryTestConfig()
	runtime := &recoveryTestRuntime{}
	runner := newRecoveryTestRunner(t, newRecoveryTestCoordinator(recoveryTestFence()),
		&recoveryTestRabbit{}, runtime)
	started := time.Now()
	if err := runner.DrainAttempts(context.Background(), config); err != nil {
		t.Fatalf("DrainAttempts() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < config.PublishAttemptTimeout-2*time.Millisecond {
		t.Fatalf("DrainAttempts returned before distributed publish bound: %s", elapsed)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.closed != 1 || runtime.drained != 1 {
		t.Fatalf("runtime close/drain = %d/%d, want 1/1", runtime.closed, runtime.drained)
	}
}

func TestRecoveryRunPropagatesAuthoritativeStateCorruptWithoutRetry(t *testing.T) {
	t.Parallel()

	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	runtime := &recoveryTestRuntime{}
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{}, runtime)
	source := &recoveryTestSource{captureErr: ErrAuthoritativeStateCorrupt}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := runner.Run(ctx, config, fence.WriterFingerprint, source, fakeOperationJournal{})
	if !errors.Is(err, ErrAuthoritativeStateCorrupt) {
		t.Fatalf("Run() error = %v, want ErrAuthoritativeStateCorrupt", err)
	}
	source.mu.Lock()
	captures := source.captures
	source.mu.Unlock()
	if captures != 1 {
		t.Fatalf("CaptureHighWater calls = %d, want 1", captures)
	}
}

func TestRecoveryRunUsesHighWaterPagesAndConvergesStableIdentity(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	rabbit := &recoveryTestRabbit{depths: map[string]int64{"active": 1, "known-only": 0}}
	runtime := &recoveryTestRuntime{}
	runner := newRecoveryTestRunner(t, coordinator, rabbit, runtime)
	now := time.Now().UTC()
	source := &recoveryTestSource{
		highWater: "100",
		known:     []TenantRef{{TenantID: "active"}, {TenantID: "known-only"}, {TenantID: "third"}},
		dispatch: []DispatchedRef{{TenantID: "active", Token: DispatchToken{
			Resource: config.Key, TaskID: "41", Generation: 2,
		}}},
		running: []RunningLease{{
			TenantID: "active", TaskID: "42", ClaimGeneration: 7,
			ObservedDBNow: now, LeaseUntil: now.Add(2 * time.Second),
		}},
	}
	if err := runner.RunRecovery(context.Background(), config, fence, source); err != nil {
		t.Fatalf("RunRecovery() error = %v", err)
	}
	token, _ := StableReservationToken(config.Key, "42", 7)
	coordinator.mu.Lock()
	_, hasStable := coordinator.stable[token]
	active := append([]string(nil), coordinator.active...)
	passes := append([]recoveryPassUpdate(nil), coordinator.passes...)
	reset := coordinator.reset
	coordinator.mu.Unlock()
	if !hasStable || reset != 1 {
		t.Fatalf("stable restored=%v reset=%d", hasStable, reset)
	}
	for _, tenant := range active {
		if tenant == "known-only" || tenant == "third" {
			t.Fatalf("known-only tenant became active: %v", active)
		}
	}
	if len(active) == 0 || active[0] != "active" {
		t.Fatalf("active tenants = %v", active)
	}
	foundZeroRunning := false
	for _, update := range passes {
		if update.pass == RecoveryPassRunning && update.diff == 0 {
			foundZeroRunning = true
		}
	}
	if !foundZeroRunning {
		t.Fatalf("running convergence was not persisted: %+v", passes)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.captures != 1 || len(source.seenHigh) == 0 {
		t.Fatalf("capture/pages = %d/%d", source.captures, len(source.seenHigh))
	}
	for _, highWater := range source.seenHigh {
		if highWater != "100" {
			t.Fatalf("page received high water %q", highWater)
		}
	}
}

func TestRecoveryRunResumesPersistedHighWaterWithoutRecapture(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	coordinator.snapshot.Progress.HighWater = "77"
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{depths: map[string]int64{}}, nil)
	source := &recoveryTestSource{highWater: "new"}
	if err := runner.RunRecovery(context.Background(), config, fence, source); err != nil {
		t.Fatalf("RunRecovery(resume) error = %v", err)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if source.captures != 0 {
		t.Fatalf("persisted high-water was recaptured %d times", source.captures)
	}
	for _, highWater := range source.seenHigh {
		if highWater != "77" {
			t.Fatalf("resumed page high water = %q", highWater)
		}
	}
}

func TestRecoveryFenceLossAfterSourcePageStopsBeforeRestore(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	coordinator.snapshot.Progress.HighWater = "20"
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{}, nil)
	source := &recoveryTestSource{known: []TenantRef{{TenantID: "tenant"}}}
	source.afterPage = func() {
		coordinator.mu.Lock()
		coordinator.failRenew = true
		coordinator.mu.Unlock()
	}
	err := runner.RunRecovery(context.Background(), config, fence, source)
	if !errors.Is(err, ErrRecoveryOwnerStale) {
		t.Fatalf("RunRecovery() error = %v, want stale owner", err)
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if len(coordinator.known) != 0 || len(coordinator.active) != 0 {
		t.Fatalf("stale owner restored state: known=%v active=%v", coordinator.known, coordinator.active)
	}
}

func TestRecoveryFinishRefusesPhysicalAttemptState(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	coordinator.cleanup.RemainingTurns = 1
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{}, nil)
	_, err := runner.FinishRecovery(context.Background(), config, fence)
	if !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("FinishRecovery() error = %v, want not ready", err)
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.finish != 0 {
		t.Fatal("FinishRecovery called Redis finish with a live turn")
	}
}

type recoveryActiveJournal struct {
	fakeOperationJournal
	record RecoveryOperationRecord
}

type recoveryCountingJournal struct {
	fakeOperationJournal
	reads int
}

func (j *recoveryCountingJournal) Read(context.Context, string, string) (RecoveryOperationRecord, bool, error) {
	j.reads++
	return RecoveryOperationRecord{}, false, nil
}

func TestRecoveryEnsureRequiresRabbitResourceTopologyProbeBeforeJournalOrRedis(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	runtime := &recoveryTestRuntime{}
	journal := &recoveryCountingJournal{}
	runner := newRecoveryTestRunner(t, coordinator, fakeRabbitClient{}, runtime)
	runner.health = newResourceHealth(nil)

	_, err := runner.EnsureResourceReady(context.Background(), config, fence.WriterFingerprint,
		&recoveryTestSource{highWater: "1"}, journal)
	if !errors.Is(err, ErrUnsupportedTopology) || !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("EnsureResourceReady() error = %v, want unsupported topology + not ready", err)
	}
	if journal.reads != 0 || coordinator.observeCount != 0 {
		t.Fatalf("missing Rabbit prober reached journal/Redis: reads=%d observes=%d", journal.reads, coordinator.observeCount)
	}
	if health := runner.health.snapshot(); health.Recovery.Startup != RecoveryStartupFailed || health.Status != HealthStatusFailed {
		t.Fatalf("missing Rabbit prober recovery health = %+v, want failed", health.Recovery)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.opened != 0 {
		t.Fatalf("missing Rabbit prober opened resource %d times", runtime.opened)
	}
}

func TestRecoveryEnsureRabbitProbeFailureKeepsGateClosedBeforeJournalOrRedis(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	runtime := &recoveryTestRuntime{}
	journal := &recoveryCountingJournal{}
	probeErr := errors.New("rabbit unavailable")
	rabbit := &recoveryTestRabbit{probeErr: probeErr}
	runner := newRecoveryTestRunner(t, coordinator, rabbit, runtime)
	runner.health = newResourceHealth(nil)

	_, err := runner.EnsureResourceReady(context.Background(), config, fence.WriterFingerprint,
		&recoveryTestSource{highWater: "1"}, journal)
	if !errors.Is(err, probeErr) || !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("EnsureResourceReady() error = %v, want probe error + not ready", err)
	}
	if journal.reads != 0 || coordinator.observeCount != 0 || rabbit.probeCalls != 1 {
		t.Fatalf("probe failure ordering: reads=%d observes=%d probes=%d", journal.reads, coordinator.observeCount, rabbit.probeCalls)
	}
	if health := runner.health.snapshot(); health.Recovery.Startup != RecoveryStartupRunning {
		t.Fatalf("transient Rabbit failure recovery health = %+v, want running", health.Recovery)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.opened != 0 {
		t.Fatalf("failed Rabbit probe opened resource %d times", runtime.opened)
	}
}

func TestRecoveryEnsureRejectsInvalidRabbitProbeFactsBeforeJournalOrRedis(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	runtime := &recoveryTestRuntime{}
	journal := &recoveryCountingJournal{}
	rabbit := &recoveryTestRabbit{probe: RabbitResourceProbe{Resource: "other.resource", DeadLetterDepth: -1}}
	runner := newRecoveryTestRunner(t, coordinator, rabbit, runtime)
	runner.health = newResourceHealth(nil)

	_, err := runner.EnsureResourceReady(context.Background(), config, fence.WriterFingerprint,
		&recoveryTestSource{highWater: "1"}, journal)
	if !errors.Is(err, ErrAuthoritativeStateCorrupt) || !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("EnsureResourceReady() error = %v, want authoritative corruption + not ready", err)
	}
	if journal.reads != 0 || coordinator.observeCount != 0 || rabbit.probeCalls != 1 {
		t.Fatalf("invalid probe facts reached journal/Redis: reads=%d observes=%d probes=%d",
			journal.reads, coordinator.observeCount, rabbit.probeCalls)
	}
	if health := runner.health.snapshot(); health.Status != HealthStatusFailed || health.Recovery.Startup != RecoveryStartupFailed {
		t.Fatalf("invalid Rabbit probe health = %+v", health)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.opened != 0 {
		t.Fatalf("invalid Rabbit probe opened resource %d times", runtime.opened)
	}
}

func TestRecoveryCoordinationCorruptionRequiresOperatorInsteadOfRetryLoop(t *testing.T) {
	t.Parallel()
	if !recoveryTerminalError(ErrCoordinationCorrupt) {
		t.Fatal("coordination corruption was classified as retryable dependency degradation")
	}
}

type recoveryInvalidJournal struct{ fakeOperationJournal }

func (recoveryInvalidJournal) Read(context.Context, string, string) (RecoveryOperationRecord, bool, error) {
	return RecoveryOperationRecord{}, false, ErrInvalidOperationRecord
}

func TestRecoveryInvalidPersistedJournalIsTerminalCoordinationCorruption(t *testing.T) {
	t.Parallel()
	runner := newRecoveryTestRunner(t, newRecoveryTestCoordinator(recoveryTestFence()),
		&recoveryTestRabbit{}, &recoveryTestRuntime{})
	_, _, err := runner.readJournal(context.Background(), recoveryInvalidJournal{},
		recoveryTestConfig().Key, recoveryTestWriter())
	if !errors.Is(err, ErrInvalidOperationRecord) || !errors.Is(err, ErrCoordinationCorrupt) ||
		!recoveryTerminalError(err) {
		t.Fatalf("readJournal() error = %v, want terminal invalid coordination state", err)
	}
}

func (j recoveryActiveJournal) Read(context.Context, string, string) (RecoveryOperationRecord, bool, error) {
	return j.record, true, nil
}

func TestRecoveryEnsureActiveJournalIsOperatorRequiredBeforeRedis(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	runtime := &recoveryTestRuntime{}
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{}, runtime)
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := RecoveryOperationRecord{
		Resource: config.Key, OperationID: strings.Repeat("d", 32), Kind: RecoveryRabbitRepair,
		Phase: OperationActive, CurrentWriterFingerprint: fence.WriterFingerprint,
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	_, err := runner.EnsureResourceReady(context.Background(), config, fence.WriterFingerprint,
		&recoveryTestSource{highWater: "1"}, recoveryActiveJournal{record: record})
	if !errors.Is(err, ErrRecoveryOperatorRequired) || !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("EnsureResourceReady() error = %v, want operator required + not ready", err)
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.observeCount != 0 {
		t.Fatal("ACTIVE journal was followed by Redis startup mutation/inspection")
	}
}

func TestRecoveryEnsureJoinsReadyOnlyAfterCanonicalPass(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	coordinator.snapshot = RecoveryControlSnapshot{}
	runtime := &recoveryTestRuntime{}
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{depths: map[string]int64{}}, runtime)
	now := time.Date(2026, 8, 3, 5, 0, 0, 0, time.UTC)
	runner.health = newResourceHealth(func() time.Time { return now })
	source := &recoveryTestSource{highWater: "1", known: []TenantRef{{TenantID: "tenant"}}}
	ready, err := runner.EnsureResourceReady(context.Background(), config, fence.WriterFingerprint,
		source, fakeOperationJournal{})
	if err != nil {
		t.Fatalf("EnsureResourceReady() error = %v", err)
	}
	if ready != fence.ResourceFence {
		t.Fatalf("ready fence = %+v, want %+v", ready, fence.ResourceFence)
	}
	coordinator.mu.Lock()
	known := append([]string(nil), coordinator.known...)
	coordinator.mu.Unlock()
	runtime.mu.Lock()
	opened := runtime.opened
	runtime.mu.Unlock()
	if opened != 1 || len(known) == 0 || known[0] != "tenant" {
		t.Fatalf("opened=%d known=%v", opened, known)
	}
	health := runner.health.snapshot()
	if health.Recovery.Startup != RecoveryStartupComplete || health.Recovery.PagesCompleted == 0 ||
		!health.Recovery.Converged || !health.Recovery.OperationPassComplete {
		t.Fatalf("successful startup recovery health = %+v", health.Recovery)
	}
	if health.Loops.Reconciler.LastSuccessAt == nil || !health.Loops.Reconciler.LastSuccessAt.Equal(now) {
		t.Fatalf("successful startup reconciliation health = %+v", health.Loops.Reconciler)
	}
}

func TestRecoveryDoesNotReportCompleteBeforeResourceGateOpens(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	openErr := errors.New("gate open failed")
	runtime := &recoveryTestRuntime{openErr: openErr}
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{depths: map[string]int64{}}, runtime)
	runner.health = newResourceHealth(nil)
	_, err := runner.EnsureResourceReady(context.Background(), config, fence.WriterFingerprint,
		&recoveryTestSource{highWater: "1"}, fakeOperationJournal{})
	if !errors.Is(err, openErr) {
		t.Fatalf("EnsureResourceReady() error = %v, want gate-open failure", err)
	}
	health := runner.health.snapshot()
	if health.Recovery.Startup == RecoveryStartupComplete || health.GateOpen {
		t.Fatalf("failed gate open reported completed recovery: %+v", health)
	}
}

type recoveryStartSession struct {
	events *[]string
	record *RecoveryOperationRecord
}

func (s recoveryStartSession) Read(context.Context) (RecoveryOperationRecord, bool, error) {
	*s.events = append(*s.events, "journal:read-start")
	if s.record == nil {
		return RecoveryOperationRecord{}, false, nil
	}
	return *s.record, true, nil
}

func (s recoveryStartSession) BeginSpecial(context.Context, *RecoveryOperationRecord, RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	return RecoveryOperationRecord{}, errors.New("unexpected BeginSpecial")
}

type recoveryStartJournal struct {
	fakeOperationJournal
	events *[]string
	record *RecoveryOperationRecord
}

func (j recoveryStartJournal) WithStartFence(_ context.Context, _ string, _ string, fn func(OperationStartSession) error) error {
	*j.events = append(*j.events, "mysql:start")
	err := fn(recoveryStartSession{events: j.events, record: j.record})
	*j.events = append(*j.events, "mysql:end")
	return err
}

func TestRecoveryNormalStartUsesMySQLThenRawLockAndEndsAfterBegin(t *testing.T) {
	fence := recoveryTestFence()
	events := []string{}
	coordinator := newRecoveryTestCoordinator(fence)
	coordinator.snapshot = RecoveryControlSnapshot{}
	coordinator.events = &events
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{}, nil)
	runner.tokens = recoveryStaticTokenSource{token: strings.Repeat("e", 32)}

	got, err := runner.beginNormalRecovery(context.Background(), recoveryTestConfig().Key,
		fence.WriterFingerprint, recoveryStartJournal{events: &events})
	if err != nil {
		t.Fatalf("beginNormalRecovery() error = %v", err)
	}
	if got.OwnerToken != strings.Repeat("e", 32) || got.Kind != RecoveryNormal {
		t.Fatalf("begin fence = %+v", got)
	}
	want := []string{
		"mysql:start", "journal:read-start", "redis:acquire", "redis:inspect",
		"journal:read-start", "redis:renew-raw", "redis:check-raw", "redis:begin", "mysql:end",
	}
	if strings.Join(events, ",") != strings.Join(want, ",") {
		t.Fatalf("start order = %v, want %v", events, want)
	}
}

type recoveryTerminalJournal struct {
	fakeOperationJournal
	mu     sync.Mutex
	events *[]string
	record RecoveryOperationRecord
}

func (j *recoveryTerminalJournal) Read(context.Context, string, string) (RecoveryOperationRecord, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.record, true, nil
}

func (j *recoveryTerminalJournal) WithStartFence(_ context.Context, _ string, _ string, fn func(OperationStartSession) error) error {
	j.mu.Lock()
	*j.events = append(*j.events, "mysql:start")
	j.mu.Unlock()
	err := fn(recoveryStartSession{events: j.events, record: &j.record})
	j.mu.Lock()
	*j.events = append(*j.events, "mysql:end")
	j.mu.Unlock()
	return err
}

func (j *recoveryTerminalJournal) Complete(_ context.Context, expected RecoveryOperationRecord) (RecoveryOperationRecord, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	*j.events = append(*j.events, "journal:complete")
	if expected.OperationID != j.record.OperationID || expected.Phase != OperationReadyCommitted {
		return RecoveryOperationRecord{}, errors.New("unexpected Complete CAS")
	}
	j.record.Phase = OperationCompleted
	j.record.Version++
	j.record.UpdatedAt = j.record.UpdatedAt.Add(time.Millisecond)
	return j.record, nil
}

func recoveryTerminalRecord(phase OperationPhase) RecoveryOperationRecord {
	now := time.Now().UTC().Truncate(time.Millisecond)
	highWater := "99"
	return RecoveryOperationRecord{
		Resource: recoveryTestConfig().Key, OperationID: strings.Repeat("d", 32),
		Kind: RecoveryRabbitRepair, Phase: phase,
		CurrentWriterFingerprint: recoveryTestWriter(), RepairHighWater: &highWater,
		RepairPassComplete: true, Version: 3, CreatedAt: now, UpdatedAt: now,
	}
}

func TestRecoveryReadyCommittedCompletesInsideStartFence(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	events := []string{}
	coordinator := newRecoveryTestCoordinator(fence)
	coordinator.events = &events
	coordinator.snapshot = RecoveryControlSnapshot{
		Present: true, State: ResourceReady, Epoch: fence.Epoch,
		ProtocolVersion: ProtocolVersion, WriterFingerprint: fence.WriterFingerprint,
		Kind: RecoveryNone, LastCompletedOperationID: strings.Repeat("d", 32),
		LastCompletedOperationKind: RecoveryRabbitRepair,
	}
	runtime := &recoveryTestRuntime{}
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{depths: map[string]int64{}}, runtime)
	runner.tokens = recoveryStaticTokenSource{token: strings.Repeat("e", 32)}
	journal := &recoveryTerminalJournal{events: &events, record: recoveryTerminalRecord(OperationReadyCommitted)}
	_, err := runner.EnsureResourceReady(context.Background(), config, fence.WriterFingerprint,
		&recoveryTestSource{highWater: "100"}, journal)
	if err != nil {
		t.Fatalf("EnsureResourceReady() error = %v", err)
	}
	complete := slicesIndex(events, "journal:complete")
	end := slicesIndex(events, "mysql:end")
	if complete < 0 || end < 0 || complete >= end {
		t.Fatalf("terminal order = %v; Complete must precede start-fence release", events)
	}
}

func TestRecoveryReadyCommittedRejectsWrongCompletedKind(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	coordinator.snapshot = RecoveryControlSnapshot{
		Present: true, State: ResourceReady, Epoch: fence.Epoch,
		ProtocolVersion: ProtocolVersion, WriterFingerprint: fence.WriterFingerprint,
		Kind: RecoveryNone, LastCompletedOperationID: strings.Repeat("d", 32),
		LastCompletedOperationKind: RecoveryWriterRebind,
	}
	runtime := &recoveryTestRuntime{}
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{depths: map[string]int64{}}, runtime)
	runner.tokens = recoveryStaticTokenSource{token: strings.Repeat("e", 32)}
	events := []string{}
	journal := &recoveryTerminalJournal{events: &events, record: recoveryTerminalRecord(OperationReadyCommitted)}
	_, err := runner.EnsureResourceReady(context.Background(), config, fence.WriterFingerprint,
		&recoveryTestSource{highWater: "100"}, journal)
	if !errors.Is(err, ErrRecoveryOperatorRequired) {
		t.Fatalf("EnsureResourceReady() error = %v, want operator-required kind mismatch", err)
	}
	if slicesIndex(events, "journal:complete") >= 0 {
		t.Fatalf("wrong completed kind advanced journal: %v", events)
	}
}

func slicesIndex(values []string, want string) int {
	for index, value := range values {
		if value == want {
			return index
		}
	}
	return -1
}

func TestRecoveryCompletedHistoricalOperationAllowsNewReadyEpochJoin(t *testing.T) {
	config := recoveryTestConfig()
	fence := recoveryTestFence()
	coordinator := newRecoveryTestCoordinator(fence)
	runtime := &recoveryTestRuntime{}
	runner := newRecoveryTestRunner(t, coordinator, &recoveryTestRabbit{depths: map[string]int64{}}, runtime)
	journal := &recoveryTerminalJournal{events: &[]string{}, record: recoveryTerminalRecord(OperationCompleted)}
	ready, err := runner.EnsureResourceReady(context.Background(), config, fence.WriterFingerprint,
		&recoveryTestSource{highWater: "100"}, journal)
	if err != nil || ready != fence.ResourceFence {
		t.Fatalf("EnsureResourceReady(COMPLETED history) = %+v, %v", ready, err)
	}
	// A COMPLETED record is historical safety audit state. A later NORMAL
	// rebuild is not required to copy its old operation ID into the new READY
	// control, so joining does not invoke raw-lock terminal reconciliation.
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if slicesIndex(*journal.events, "mysql:start") >= 0 {
		t.Fatalf("historical COMPLETED record triggered terminal reconciliation: %v", *journal.events)
	}
}
