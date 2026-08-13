package fairqueue

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errDispatcherTest = errors.New("dispatcher test failure")

type dispatcherEventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *dispatcherEventLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *dispatcherEventLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

type dispatcherSourceFake struct {
	listFn func(context.Context, string, int) ([]DispatchCandidate, string, error)
	getFn  func(context.Context, string) (DispatchCandidate, bool, error)
	markFn func(context.Context, DispatchCandidate) (bool, error)

	listCalls atomic.Int64
	getCalls  atomic.Int64
	markCalls atomic.Int64
}

func (f *dispatcherSourceFake) ListDispatchCandidates(ctx context.Context, after string, limit int) ([]DispatchCandidate, string, error) {
	f.listCalls.Add(1)
	if f.listFn == nil {
		return nil, "", nil
	}
	return f.listFn(ctx, after, limit)
}

func (f *dispatcherSourceFake) GetDispatchableByID(ctx context.Context, taskID string) (DispatchCandidate, bool, error) {
	f.getCalls.Add(1)
	if f.getFn == nil {
		return DispatchCandidate{}, false, nil
	}
	return f.getFn(ctx, taskID)
}

func (f *dispatcherSourceFake) MarkDispatched(ctx context.Context, candidate DispatchCandidate) (bool, error) {
	f.markCalls.Add(1)
	if f.markFn == nil {
		return true, nil
	}
	return f.markFn(ctx, candidate)
}

type dispatcherRearmFake struct {
	rearmFn func(context.Context, string, int) ([]DispatchCandidate, string, error)
	calls   atomic.Int64
}

func (f *dispatcherRearmFake) RearmExpiredPage(ctx context.Context, after string, limit int) ([]DispatchCandidate, string, error) {
	f.calls.Add(1)
	if f.rearmFn == nil {
		return nil, "", nil
	}
	return f.rearmFn(ctx, after, limit)
}

type dispatcherRabbitFake struct {
	RabbitClient
	publishFn func(context.Context, Message) (PublishReceipt, error)
	calls     atomic.Int64

	mu       sync.Mutex
	messages []Message
}

func (f *dispatcherRabbitFake) PublishMandatoryConfirmed(ctx context.Context, message Message) (PublishReceipt, error) {
	f.calls.Add(1)
	f.mu.Lock()
	f.messages = append(f.messages, message)
	f.mu.Unlock()
	if f.publishFn == nil {
		return PublishReceipt{AttemptID: strings.Repeat("a", 32)}, nil
	}
	return f.publishFn(ctx, message)
}

func (f *dispatcherRabbitFake) publishedMessages() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Message(nil), f.messages...)
}

type dispatcherCoordinatorFake struct {
	Coordinator
	checkFn    func(context.Context, string, ResourceFence) error
	activateFn func(context.Context, string, ResourceFence, string) error
	ensureFn   func(context.Context, string, ResourceFence, string) error
	activeFn   func(context.Context, string, ResourceFence, string) error

	checkCalls    atomic.Int64
	activateCalls atomic.Int64
	ensureCalls   atomic.Int64
	activeCalls   atomic.Int64
}

func (f *dispatcherCoordinatorFake) CheckReadyFence(ctx context.Context, resource string, fence ResourceFence) error {
	f.checkCalls.Add(1)
	if f.checkFn == nil {
		return nil
	}
	return f.checkFn(ctx, resource, fence)
}

func (f *dispatcherCoordinatorFake) Activate(ctx context.Context, resource string, fence ResourceFence, tenant string) error {
	f.activateCalls.Add(1)
	f.activeCalls.Add(1)
	if f.activateFn != nil {
		return f.activateFn(ctx, resource, fence, tenant)
	}
	if f.activeFn != nil {
		return f.activeFn(ctx, resource, fence, tenant)
	}
	return nil
}

func (f *dispatcherCoordinatorFake) EnsureActive(ctx context.Context, resource string, fence ResourceFence, tenant string) error {
	f.ensureCalls.Add(1)
	f.activeCalls.Add(1)
	if f.ensureFn != nil {
		return f.ensureFn(ctx, resource, fence, tenant)
	}
	if f.activeFn == nil {
		return nil
	}
	return f.activeFn(ctx, resource, fence, tenant)
}

func dispatcherTestCandidate(taskID, guard string, generation uint64) DispatchCandidate {
	return DispatchCandidate{
		Message: Message{
			Version:  MessageVersion1,
			Resource: "rag.index",
			TenantID: "tenant-a",
			TaskType: "rag_index",
			TaskID:   taskID,
			DispatchToken: DispatchToken{
				Resource: "rag.index", TaskID: taskID, Generation: generation,
			},
		},
		Guard: guard,
	}
}

func dispatcherTestFence(seed string) ResourceFence {
	return ResourceFence{
		Epoch:             strings.Repeat(seed, 32),
		WriterFingerprint: strings.Repeat(seed, 64),
	}
}

func dispatcherTestOptions() DispatcherOptions {
	return DispatcherOptions{
		PageSize:                    2,
		DispatchInterval:            20 * time.Millisecond,
		ExpiredRunningSweepInterval: 20 * time.Millisecond,
		PublishAttemptTimeout:       200 * time.Millisecond,
		BackoffInitial:              5 * time.Millisecond,
		BackoffMax:                  20 * time.Millisecond,
	}
}

func newDispatcherForTest(t *testing.T, source DispatchSource, rearm ExpiredRearmSource, rabbit RabbitClient, coordinator Coordinator, options DispatcherOptions) *Dispatcher {
	t.Helper()
	fence := dispatcherTestFence("a")
	dispatcher, err := NewDispatcher("rag.index", fence, source, rearm, rabbit, coordinator, options)
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	if err := dispatcher.OpenPublisherGate(fence); err != nil {
		t.Fatalf("OpenPublisherGate() error = %v", err)
	}
	return dispatcher
}

func TestDispatcherStartsWithClosedPublisherGate(t *testing.T) {
	t.Parallel()

	source := &dispatcherSourceFake{}
	rearm := &dispatcherRearmFake{}
	rabbit := &dispatcherRabbitFake{}
	coordinator := &dispatcherCoordinatorFake{}
	dispatcher, err := NewDispatcher(
		"rag.index",
		dispatcherTestFence("a"),
		source,
		rearm,
		rabbit,
		coordinator,
		dispatcherTestOptions(),
	)
	if err != nil {
		t.Fatalf("NewDispatcher() error = %v", err)
	}
	if dispatcher.PublisherGateOpen() {
		t.Fatal("new dispatcher publisher gate is open before READY reconciliation")
	}
	if _, err := dispatcher.DispatchPage(context.Background(), ""); !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("DispatchPage(closed by default) error = %v, want ErrResourceNotReady", err)
	}
	if source.listCalls.Load() != 0 || rearm.calls.Load() != 0 || rabbit.calls.Load() != 0 || coordinator.checkCalls.Load() != 0 {
		t.Fatalf("closed-by-default dispatcher touched dependencies: list=%d rearm=%d publish=%d check=%d",
			source.listCalls.Load(), rearm.calls.Load(), rabbit.calls.Load(), coordinator.checkCalls.Load())
	}
}

func TestDispatcherStrictOrderAndSharedSingleCandidatePath(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		lookupCall string
		run        func(context.Context, *Dispatcher) error
	}{
		{
			name:       "inline by ID",
			lookupCall: "get",
			run: func(ctx context.Context, dispatcher *Dispatcher) error {
				_, err := dispatcher.TryDispatch(ctx, "task-1")
				return err
			},
		},
		{
			name:       "keyset page",
			lookupCall: "list",
			run: func(ctx context.Context, dispatcher *Dispatcher) error {
				next, err := dispatcher.DispatchPage(ctx, "cursor-0")
				if next != "cursor-1" {
					t.Fatalf("DispatchPage() next = %q, want cursor-1", next)
				}
				return err
			},
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := dispatcherTestCandidate("task-1", `{"status":"PENDING","retry":3}`, 7)
			var events dispatcherEventLog
			source := &dispatcherSourceFake{
				getFn: func(_ context.Context, taskID string) (DispatchCandidate, bool, error) {
					events.add("get")
					if taskID != candidate.Message.TaskID {
						t.Fatalf("GetDispatchableByID() taskID = %q", taskID)
					}
					return candidate, true, nil
				},
				listFn: func(_ context.Context, after string, limit int) ([]DispatchCandidate, string, error) {
					events.add("list")
					if after != "cursor-0" || limit != 2 {
						t.Fatalf("ListDispatchCandidates() = after %q limit %d", after, limit)
					}
					return []DispatchCandidate{candidate}, "cursor-1", nil
				},
				markFn: func(_ context.Context, got DispatchCandidate) (bool, error) {
					events.add("mark")
					if !reflect.DeepEqual(got, candidate) {
						t.Fatalf("MarkDispatched() candidate = %#v, want original %#v", got, candidate)
					}
					return true, nil
				},
			}
			rabbit := &dispatcherRabbitFake{publishFn: func(_ context.Context, got Message) (PublishReceipt, error) {
				events.add("publish")
				if !reflect.DeepEqual(got, candidate.Message) {
					t.Fatalf("PublishMandatoryConfirmed() message = %#v", got)
				}
				return PublishReceipt{AttemptID: strings.Repeat("b", 32)}, nil
			}}
			coordinator := &dispatcherCoordinatorFake{
				checkFn: func(_ context.Context, resource string, fence ResourceFence) error {
					events.add("check")
					if resource != candidate.Message.Resource || fence != dispatcherTestFence("a") {
						t.Fatalf("CheckReadyFence() = %q, %#v", resource, fence)
					}
					return nil
				},
				activeFn: func(_ context.Context, resource string, fence ResourceFence, tenant string) error {
					events.add("active")
					if resource != candidate.Message.Resource || tenant != candidate.Message.TenantID || fence != dispatcherTestFence("a") {
						t.Fatalf("EnsureActive() = %q, %#v, %q", resource, fence, tenant)
					}
					return nil
				},
			}

			dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, rabbit, coordinator, dispatcherTestOptions())
			if err := test.run(context.Background(), dispatcher); err != nil {
				t.Fatalf("dispatch error = %v", err)
			}
			want := []string{test.lookupCall, "check", "publish", "mark", "active"}
			if got := events.snapshot(); !reflect.DeepEqual(got, want) {
				t.Fatalf("events = %v, want %v", got, want)
			}
			if coordinator.activateCalls.Load() != 1 || coordinator.ensureCalls.Load() != 0 {
				t.Fatalf("mark=true activation/ensure calls = %d/%d, want 1/0", coordinator.activateCalls.Load(), coordinator.ensureCalls.Load())
			}
		})
	}
}

func TestDispatcherReadyCheckFailureClosesGateBeforeRabbit(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-check", "original-guard", 1)
	source := &dispatcherSourceFake{
		getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
			return candidate, true, nil
		},
	}
	rabbit := &dispatcherRabbitFake{}
	coordinator := &dispatcherCoordinatorFake{checkFn: func(context.Context, string, ResourceFence) error {
		return ErrResourceNotReady
	}}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, rabbit, coordinator, dispatcherTestOptions())

	_, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID)
	if !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("TryDispatch() error = %v, want ErrResourceNotReady", err)
	}
	if rabbit.calls.Load() != 0 || source.markCalls.Load() != 0 || coordinator.activeCalls.Load() != 0 {
		t.Fatalf("failure touched downstream: publish=%d mark=%d active=%d", rabbit.calls.Load(), source.markCalls.Load(), coordinator.activeCalls.Load())
	}
	if dispatcher.PublisherGateOpen() {
		t.Fatal("publisher gate remained open after READY fence failure")
	}
}

func TestDispatcherAuthoritativeWriterMismatchClosesGateAndStopsRun(t *testing.T) {
	t.Parallel()

	source := &dispatcherSourceFake{listFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		return nil, "", ErrAuthoritativeWriterMismatch
	}}
	rabbit := &dispatcherRabbitFake{}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, rabbit, &dispatcherCoordinatorFake{}, dispatcherTestOptions())

	err := dispatcher.Run(context.Background())
	if !errors.Is(err, ErrAuthoritativeWriterMismatch) {
		t.Fatalf("Run() error = %v, want ErrAuthoritativeWriterMismatch", err)
	}
	if dispatcher.PublisherGateOpen() {
		t.Fatal("fatal writer mismatch left publisher gate open")
	}
	if rabbit.calls.Load() != 0 {
		t.Fatalf("fatal source read published %d messages", rabbit.calls.Load())
	}
}

func TestDispatcherAuthoritativeStateCorruptClosesGateForExactGet(t *testing.T) {
	t.Parallel()

	source := &dispatcherSourceFake{getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
		return DispatchCandidate{}, false, ErrAuthoritativeStateCorrupt
	}}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, dispatcherTestOptions())

	found, err := dispatcher.TryDispatch(context.Background(), "task-corrupt-get")
	if found || !errors.Is(err, ErrAuthoritativeStateCorrupt) {
		t.Fatalf("TryDispatch() = %v, %v, want false/ErrAuthoritativeStateCorrupt", found, err)
	}
	if dispatcher.PublisherGateOpen() {
		t.Fatal("authoritative Get corruption left publisher gate open")
	}
	if source.getCalls.Load() != 1 {
		t.Fatalf("GetDispatchableByID calls = %d, want 1", source.getCalls.Load())
	}
}

func TestDispatcherAuthoritativeStateCorruptListStopsRun(t *testing.T) {
	t.Parallel()

	source := &dispatcherSourceFake{listFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		return nil, "", ErrAuthoritativeStateCorrupt
	}}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, dispatcherTestOptions())
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := dispatcher.Run(ctx)
	if !errors.Is(err, ErrAuthoritativeStateCorrupt) {
		t.Fatalf("Run() error = %v, want ErrAuthoritativeStateCorrupt", err)
	}
	if dispatcher.PublisherGateOpen() {
		t.Fatal("authoritative List corruption left publisher gate open")
	}
	if source.listCalls.Load() != 1 {
		t.Fatalf("ListDispatchCandidates calls = %d, want 1", source.listCalls.Load())
	}
}

func TestDispatcherFatalMarkClosesGateAfterConfirmedPublish(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-fatal-mark", "guard-fatal-mark", 15)
	source := &dispatcherSourceFake{
		getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
			return candidate, true, nil
		},
		markFn: func(context.Context, DispatchCandidate) (bool, error) {
			return false, ErrAuthoritativeWriterMismatch
		},
	}
	coordinator := &dispatcherCoordinatorFake{}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, coordinator, dispatcherTestOptions())

	if _, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID); !errors.Is(err, ErrAuthoritativeWriterMismatch) {
		t.Fatalf("TryDispatch() error = %v, want ErrAuthoritativeWriterMismatch", err)
	}
	if dispatcher.PublisherGateOpen() {
		t.Fatal("fatal Mark left publisher gate open")
	}
	if source.markCalls.Load() != 1 || coordinator.ensureCalls.Load() != 0 || coordinator.activateCalls.Load() != 0 {
		t.Fatalf("fatal Mark calls mark/ensure/activate = %d/%d/%d, want 1/0/0",
			source.markCalls.Load(), coordinator.ensureCalls.Load(), coordinator.activateCalls.Load())
	}
}

func TestDispatcherAuthoritativeStateCorruptMarkStopsAfterConfirmedPublish(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-corrupt-mark", "guard-corrupt-mark", 16)
	source := &dispatcherSourceFake{
		listFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
			return []DispatchCandidate{candidate}, "", nil
		},
		markFn: func(context.Context, DispatchCandidate) (bool, error) {
			return false, ErrAuthoritativeStateCorrupt
		},
	}
	rabbit := &dispatcherRabbitFake{}
	coordinator := &dispatcherCoordinatorFake{}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, rabbit, coordinator, dispatcherTestOptions())
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := dispatcher.Run(ctx)
	if !errors.Is(err, ErrAuthoritativeStateCorrupt) {
		t.Fatalf("Run() error = %v, want ErrAuthoritativeStateCorrupt", err)
	}
	if dispatcher.PublisherGateOpen() {
		t.Fatal("authoritative Mark corruption left publisher gate open")
	}
	if rabbit.calls.Load() != 1 || source.markCalls.Load() != 1 || coordinator.ensureCalls.Load() != 0 {
		t.Fatalf("corrupt Mark calls publish/mark/ensure = %d/%d/%d, want 1/1/0",
			rabbit.calls.Load(), source.markCalls.Load(), coordinator.ensureCalls.Load())
	}
}

func TestDispatcherLateFatalSourceCannotCloseReopenedSameFence(t *testing.T) {
	t.Parallel()

	listStarted := make(chan struct{})
	releaseList := make(chan struct{})
	source := &dispatcherSourceFake{listFn: func(ctx context.Context, _ string, _ int) ([]DispatchCandidate, string, error) {
		close(listStarted)
		select {
		case <-releaseList:
			return nil, "", ErrAuthoritativeWriterMismatch
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, dispatcherTestOptions())

	done := make(chan error, 1)
	go func() {
		_, err := dispatcher.DispatchPage(context.Background(), "")
		done <- err
	}()
	select {
	case <-listStarted:
	case <-time.After(time.Second):
		t.Fatal("dispatch source read did not start")
	}
	dispatcher.ClosePublisherGate()
	if err := dispatcher.OpenPublisherGate(dispatcherTestFence("a")); err != nil {
		t.Fatalf("OpenPublisherGate(same fence) error = %v", err)
	}
	close(releaseList)
	select {
	case err := <-done:
		if !errors.Is(err, ErrAuthoritativeWriterMismatch) {
			t.Fatalf("DispatchPage() error = %v, want ErrAuthoritativeWriterMismatch", err)
		}
	case <-time.After(time.Second):
		t.Fatal("late source read did not finish")
	}
	if !dispatcher.PublisherGateOpen() {
		t.Fatal("late old-generation source failure closed same-fence reopened gate")
	}
}

func TestDispatcherLateMixedAuthoritativeFailureIsNeverStaleExempted(t *testing.T) {
	t.Parallel()

	listStarted := make(chan struct{})
	releaseList := make(chan struct{})
	mixedFailure := errors.Join(ErrAuthoritativeWriterMismatch, ErrAuthoritativeStateCorrupt)
	source := &dispatcherSourceFake{listFn: func(ctx context.Context, _ string, _ int) ([]DispatchCandidate, string, error) {
		close(listStarted)
		select {
		case <-releaseList:
			return nil, "", mixedFailure
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, dispatcherTestOptions())

	done := make(chan error, 1)
	go func() {
		_, err := dispatcher.DispatchPage(context.Background(), "")
		done <- err
	}()
	select {
	case <-listStarted:
	case <-time.After(time.Second):
		t.Fatal("dispatch source read did not start")
	}
	dispatcher.ClosePublisherGate()
	if err := dispatcher.OpenPublisherGate(dispatcherTestFence("a")); err != nil {
		t.Fatalf("OpenPublisherGate(same fence) error = %v", err)
	}
	close(releaseList)
	select {
	case err := <-done:
		if !errors.Is(err, ErrAuthoritativeWriterMismatch) || !errors.Is(err, ErrAuthoritativeStateCorrupt) {
			t.Fatalf("DispatchPage() error = %v, want mixed authoritative failure", err)
		}
		if isStalePublisherSourceFailure(err) {
			t.Fatalf("mixed authoritative corruption was stale-exempted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("late source read did not finish")
	}
	if !dispatcher.PublisherGateOpen() {
		t.Fatal("late old-generation source failure closed same-fence reopened gate")
	}
}

func TestDispatcherClosedGateSkipsSourceAndReopensWithNewFence(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-gate", "guard-gate", 12)
	source := &dispatcherSourceFake{getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
		return candidate, true, nil
	}}
	var mu sync.Mutex
	var checked []ResourceFence
	coordinator := &dispatcherCoordinatorFake{checkFn: func(_ context.Context, _ string, fence ResourceFence) error {
		mu.Lock()
		checked = append(checked, fence)
		mu.Unlock()
		return nil
	}}
	rabbit := &dispatcherRabbitFake{}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, rabbit, coordinator, dispatcherTestOptions())

	dispatcher.ClosePublisherGate()
	found, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID)
	if found || !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("TryDispatch(closed) = %v, %v, want false/ErrResourceNotReady", found, err)
	}
	if source.getCalls.Load() != 0 || source.listCalls.Load() != 0 || rabbit.calls.Load() != 0 {
		t.Fatalf("closed gate touched source/Rabbit: get=%d list=%d publish=%d", source.getCalls.Load(), source.listCalls.Load(), rabbit.calls.Load())
	}

	newFence := dispatcherTestFence("b")
	if err := dispatcher.OpenPublisherGate(newFence); err != nil {
		t.Fatalf("OpenPublisherGate() error = %v", err)
	}
	if !dispatcher.PublisherGateOpen() {
		t.Fatal("publisher gate did not reopen")
	}
	found, err = dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID)
	if !found || err != nil {
		t.Fatalf("TryDispatch(reopened) = %v, %v, want true/nil", found, err)
	}
	mu.Lock()
	gotChecked := append([]ResourceFence(nil), checked...)
	mu.Unlock()
	if !reflect.DeepEqual(gotChecked, []ResourceFence{newFence}) {
		t.Fatalf("checked fences = %#v, want only new fence", gotChecked)
	}
}

func TestDispatcherGateDrainsThroughMarkAndOldActivationCannotCloseReopenedFence(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-drain", "guard-drain", 13)
	markStarted := make(chan struct{})
	releaseMark := make(chan struct{})
	source := &dispatcherSourceFake{
		getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
			return candidate, true, nil
		},
		markFn: func(ctx context.Context, _ DispatchCandidate) (bool, error) {
			close(markStarted)
			select {
			case <-releaseMark:
				return true, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		},
	}
	activationStarted := make(chan struct{})
	releaseActivation := make(chan struct{})
	var activationFence ResourceFence
	coordinator := &dispatcherCoordinatorFake{activateFn: func(ctx context.Context, _ string, fence ResourceFence, _ string) error {
		activationFence = fence
		close(activationStarted)
		select {
		case <-releaseActivation:
			return ErrDependencyUnavailable
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	options := dispatcherTestOptions()
	options.PublishAttemptTimeout = time.Second
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, coordinator, options)

	dispatchDone := make(chan error, 1)
	go func() {
		_, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID)
		dispatchDone <- err
	}()
	select {
	case <-markStarted:
	case <-time.After(time.Second):
		t.Fatal("dispatch did not reach MarkDispatched")
	}

	dispatcher.ClosePublisherGate()
	reopenedFence := dispatcherTestFence("a")
	if err := dispatcher.OpenPublisherGate(reopenedFence); !errors.Is(err, ErrResourceNotReady) {
		t.Fatalf("OpenPublisherGate(in-flight) error = %v, want ErrResourceNotReady", err)
	}
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 25*time.Millisecond)
	err := dispatcher.WaitForPublisherDrain(drainCtx)
	cancelDrain()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitForPublisherDrain(marking) error = %v, want deadline exceeded", err)
	}

	close(releaseMark)
	select {
	case <-activationStarted:
	case <-time.After(time.Second):
		t.Fatal("dispatch did not reach independent activation")
	}
	if err := dispatcher.WaitForPublisherDrain(context.Background()); err != nil {
		t.Fatalf("WaitForPublisherDrain() while activation is blocked error = %v", err)
	}
	if err := dispatcher.OpenPublisherGate(reopenedFence); err != nil {
		t.Fatalf("OpenPublisherGate(after Mark drain) error = %v", err)
	}
	close(releaseActivation)
	select {
	case err := <-dispatchDone:
		if !errors.Is(err, ErrDependencyUnavailable) {
			t.Fatalf("old-fence activation error = %v, want ErrDependencyUnavailable", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old-fence activation did not finish")
	}
	if activationFence != dispatcherTestFence("a") {
		t.Fatalf("activation fence = %#v, want old admitted fence", activationFence)
	}
	if !dispatcher.PublisherGateOpen() {
		t.Fatal("late activation failure closed a gate reopened with the same fence")
	}
}

func TestDispatcherPublishFailuresDoNotMarkOrActivate(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "basic return", err: ErrPublishUnroutable},
		{name: "confirm nack", err: ErrPublishUnconfirmed},
		{name: "attempt timeout", err: context.DeadlineExceeded},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			candidate := dispatcherTestCandidate("task-publish", "guard-publish", 2)
			source := &dispatcherSourceFake{getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
				return candidate, true, nil
			}}
			rabbit := &dispatcherRabbitFake{publishFn: func(ctx context.Context, _ Message) (PublishReceipt, error) {
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("publish context has no attempt deadline")
				}
				return PublishReceipt{AttemptID: strings.Repeat("c", 32)}, test.err
			}}
			coordinator := &dispatcherCoordinatorFake{}
			dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, rabbit, coordinator, dispatcherTestOptions())

			_, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID)
			if !errors.Is(err, test.err) {
				t.Fatalf("TryDispatch() error = %v, want %v", err, test.err)
			}
			if source.markCalls.Load() != 0 || coordinator.activeCalls.Load() != 0 {
				t.Fatalf("publish failure touched mark/active: %d/%d", source.markCalls.Load(), coordinator.activeCalls.Load())
			}
		})
	}
}

func TestDispatcherUsesOneDeadlineForCheckPublishAndMark(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-deadline", "guard-deadline", 3)
	var mu sync.Mutex
	deadlines := make([]time.Time, 0, 3)
	recordDeadline := func(ctx context.Context) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("attempt step has no deadline")
		}
		mu.Lock()
		deadlines = append(deadlines, deadline)
		mu.Unlock()
	}
	source := &dispatcherSourceFake{
		getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
			return candidate, true, nil
		},
		markFn: func(ctx context.Context, got DispatchCandidate) (bool, error) {
			recordDeadline(ctx)
			if !reflect.DeepEqual(got, candidate) {
				t.Fatal("mark did not receive the source candidate")
			}
			return true, nil
		},
	}
	rabbit := &dispatcherRabbitFake{publishFn: func(ctx context.Context, _ Message) (PublishReceipt, error) {
		recordDeadline(ctx)
		return PublishReceipt{AttemptID: strings.Repeat("d", 32)}, nil
	}}
	coordinator := &dispatcherCoordinatorFake{checkFn: func(ctx context.Context, _ string, _ ResourceFence) error {
		recordDeadline(ctx)
		return nil
	}}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, rabbit, coordinator, dispatcherTestOptions())

	if _, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID); err != nil {
		t.Fatalf("TryDispatch() error = %v", err)
	}
	mu.Lock()
	got := append([]time.Time(nil), deadlines...)
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("captured %d deadlines, want 3", len(got))
	}
	if !got[0].Equal(got[1]) || !got[0].Equal(got[2]) {
		t.Fatalf("attempt deadlines differ: %v", got)
	}
}

func TestDispatcherMarkDeadlineBestEffortEnsuresActiveWithoutActivate(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-blocked-mark", "guard-blocked-mark", 4)
	markStarted := make(chan struct{})
	source := &dispatcherSourceFake{
		getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
			return candidate, true, nil
		},
		markFn: func(ctx context.Context, _ DispatchCandidate) (bool, error) {
			close(markStarted)
			<-ctx.Done()
			return false, ctx.Err()
		},
	}
	var activationSawLiveContext atomic.Bool
	coordinator := &dispatcherCoordinatorFake{ensureFn: func(ctx context.Context, _ string, _ ResourceFence, _ string) error {
		activationSawLiveContext.Store(ctx.Err() == nil)
		return nil
	}}
	options := dispatcherTestOptions()
	options.PublishAttemptTimeout = 35 * time.Millisecond
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, coordinator, options)

	startedAt := time.Now()
	_, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("TryDispatch() error = %v, want deadline exceeded", err)
	}
	select {
	case <-markStarted:
	default:
		t.Fatal("MarkDispatched was not called")
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("blocked mark exceeded bounded attempt timeout: %v", elapsed)
	}
	if coordinator.activateCalls.Load() != 0 || coordinator.ensureCalls.Load() != 1 {
		t.Fatalf("timed-out mark activate/ensure calls = %d/%d, want 0/1", coordinator.activateCalls.Load(), coordinator.ensureCalls.Load())
	}
	if !activationSawLiveContext.Load() {
		t.Fatal("best-effort EnsureActive inherited the expired publish-attempt context")
	}
}

func TestDispatcherActivationUsesIndependentBoundedBudget(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-activation-budget", "guard-activation-budget", 14)
	source := &dispatcherSourceFake{
		getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
			return candidate, true, nil
		},
		markFn: func(ctx context.Context, _ DispatchCandidate) (bool, error) {
			select {
			case <-time.After(25 * time.Millisecond):
				return true, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		},
	}
	coordinator := &dispatcherCoordinatorFake{activateFn: func(ctx context.Context, _ string, _ ResourceFence, _ string) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("activation context is not bounded")
		}
		select {
		case <-time.After(25 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	options := dispatcherTestOptions()
	options.PublishAttemptTimeout = 40 * time.Millisecond
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, coordinator, options)

	found, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID)
	if !found || err != nil {
		t.Fatalf("TryDispatch() = %v, %v, want true/nil", found, err)
	}
	if coordinator.activateCalls.Load() != 1 {
		t.Fatalf("Activate() calls = %d, want 1", coordinator.activateCalls.Load())
	}
}

func TestDispatcherPublishSuccessBeforeMarkFailureCanBeRetried(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-retry", "same-original-guard", 5)
	var markAttempt atomic.Int64
	source := &dispatcherSourceFake{
		getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
			return candidate, true, nil
		},
		markFn: func(_ context.Context, got DispatchCandidate) (bool, error) {
			if !reflect.DeepEqual(got, candidate) {
				t.Fatal("retry replaced the original candidate guard")
			}
			if markAttempt.Add(1) == 1 {
				return false, errDispatcherTest
			}
			return true, nil
		},
	}
	var receiptIndex atomic.Int64
	var receiptMu sync.Mutex
	receipts := make([]string, 0, 2)
	rabbit := &dispatcherRabbitFake{publishFn: func(context.Context, Message) (PublishReceipt, error) {
		attempt := receiptIndex.Add(1)
		id := strings.Repeat(string(rune('a'+attempt)), 32)
		receiptMu.Lock()
		receipts = append(receipts, id)
		receiptMu.Unlock()
		return PublishReceipt{AttemptID: id}, nil
	}}
	coordinator := &dispatcherCoordinatorFake{}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, rabbit, coordinator, dispatcherTestOptions())

	if _, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID); !errors.Is(err, errDispatcherTest) {
		t.Fatalf("first TryDispatch() error = %v", err)
	}
	if _, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID); err != nil {
		t.Fatalf("second TryDispatch() error = %v", err)
	}
	if rabbit.calls.Load() != 2 || source.markCalls.Load() != 2 || coordinator.activeCalls.Load() != 2 {
		t.Fatalf("calls publish/mark/active = %d/%d/%d", rabbit.calls.Load(), source.markCalls.Load(), coordinator.activeCalls.Load())
	}
	if coordinator.ensureCalls.Load() != 1 || coordinator.activateCalls.Load() != 1 {
		t.Fatalf("mark-error ensure / mark-success activate = %d/%d, want 1/1", coordinator.ensureCalls.Load(), coordinator.activateCalls.Load())
	}
	receiptMu.Lock()
	gotReceipts := append([]string(nil), receipts...)
	receiptMu.Unlock()
	if len(gotReceipts) != 2 || gotReceipts[0] == gotReceipts[1] {
		t.Fatalf("wire attempts = %v, want two distinct attempt IDs", gotReceipts)
	}
	messages := rabbit.publishedMessages()
	if len(messages) != 2 || !reflect.DeepEqual(messages[0], messages[1]) {
		t.Fatalf("retry messages = %#v, want stable logical message", messages)
	}
}

func TestDispatcherMarkCASFalseStillEnsuresActive(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-stale", "stale-guard", 6)
	source := &dispatcherSourceFake{
		getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
			return candidate, true, nil
		},
		markFn: func(context.Context, DispatchCandidate) (bool, error) {
			return false, nil
		},
	}
	coordinator := &dispatcherCoordinatorFake{}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, coordinator, dispatcherTestOptions())

	if _, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID); err != nil {
		t.Fatalf("TryDispatch() error = %v", err)
	}
	if coordinator.activeCalls.Load() != 1 {
		t.Fatalf("EnsureActive() calls = %d, want 1", coordinator.activeCalls.Load())
	}
	if coordinator.ensureCalls.Load() != 1 || coordinator.activateCalls.Load() != 0 {
		t.Fatalf("mark=false ensure/activate calls = %d/%d, want 1/0", coordinator.ensureCalls.Load(), coordinator.activateCalls.Load())
	}
}

func TestDispatcherMarkSuccessIsNotRolledBackWhenRedisActivationFails(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("task-redis-fail", "guard-redis-fail", 7)
	var marked atomic.Bool
	source := &dispatcherSourceFake{
		getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
			return candidate, true, nil
		},
		markFn: func(context.Context, DispatchCandidate) (bool, error) {
			marked.Store(true)
			return true, nil
		},
	}
	coordinator := &dispatcherCoordinatorFake{activeFn: func(context.Context, string, ResourceFence, string) error {
		return ErrDependencyUnavailable
	}}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, coordinator, dispatcherTestOptions())

	_, err := dispatcher.TryDispatch(context.Background(), candidate.Message.TaskID)
	if !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("TryDispatch() error = %v, want ErrDependencyUnavailable", err)
	}
	if !marked.Load() || source.markCalls.Load() != 1 || coordinator.activeCalls.Load() != 1 {
		t.Fatalf("published state was not preserved: marked=%v mark=%d active=%d", marked.Load(), source.markCalls.Load(), coordinator.activeCalls.Load())
	}
}

func TestDispatcherTryDispatchValidatesReturnedIdentityAndNeverLists(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name      string
		candidate DispatchCandidate
	}{
		{
			name:      "different task ID",
			candidate: dispatcherTestCandidate("another-task", "guard-another-task", 15),
		},
		{
			name: "different resource",
			candidate: func() DispatchCandidate {
				candidate := dispatcherTestCandidate("requested-task", "guard-other-resource", 15)
				candidate.Message.Resource = "image.generate"
				candidate.Message.DispatchToken.Resource = "image.generate"
				return candidate
			}(),
		},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			source := &dispatcherSourceFake{
				listFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
					t.Fatal("TryDispatch called ListDispatchCandidates")
					return nil, "", nil
				},
				getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
					return test.candidate, true, nil
				},
			}
			rabbit := &dispatcherRabbitFake{}
			coordinator := &dispatcherCoordinatorFake{}
			dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, rabbit, coordinator, dispatcherTestOptions())

			found, err := dispatcher.TryDispatch(context.Background(), "requested-task")
			if !found || !errors.Is(err, ErrInvalidModel) {
				t.Fatalf("TryDispatch(mismatch) = %v, %v, want true/ErrInvalidModel", found, err)
			}
			if source.listCalls.Load() != 0 || rabbit.calls.Load() != 0 || source.markCalls.Load() != 0 {
				t.Fatalf("mismatch touched list/Rabbit/mark: %d/%d/%d", source.listCalls.Load(), rabbit.calls.Load(), source.markCalls.Load())
			}
		})
	}

	source := &dispatcherSourceFake{getFn: func(context.Context, string) (DispatchCandidate, bool, error) {
		return DispatchCandidate{}, false, nil
	}}
	dispatcher := newDispatcherForTest(t, source, &dispatcherRearmFake{}, &dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, dispatcherTestOptions())
	found, err := dispatcher.TryDispatch(context.Background(), "missing-task")
	if found || err != nil {
		t.Fatalf("TryDispatch(missing) = %v, %v, want false/nil", found, err)
	}
	if source.listCalls.Load() != 0 {
		t.Fatalf("TryDispatch(missing) list calls = %d", source.listCalls.Load())
	}
}

func TestDispatcherRearmReadyPreflightPrecedesDomainMutation(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("expired-preflight", "guard-expired-preflight", 16)
	var events dispatcherEventLog
	var checks atomic.Int64
	coordinator := &dispatcherCoordinatorFake{
		checkFn: func(context.Context, string, ResourceFence) error {
			checks.Add(1)
			events.add("check")
			return nil
		},
		activateFn: func(context.Context, string, ResourceFence, string) error {
			events.add("activate")
			return nil
		},
	}
	rearm := &dispatcherRearmFake{rearmFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		events.add("rearm")
		return []DispatchCandidate{candidate}, "", nil
	}}
	source := &dispatcherSourceFake{markFn: func(context.Context, DispatchCandidate) (bool, error) {
		events.add("mark")
		return true, nil
	}}
	rabbit := &dispatcherRabbitFake{publishFn: func(context.Context, Message) (PublishReceipt, error) {
		events.add("publish")
		return PublishReceipt{AttemptID: strings.Repeat("e", 32)}, nil
	}}
	dispatcher := newDispatcherForTest(t, source, rearm, rabbit, coordinator, dispatcherTestOptions())

	if _, err := dispatcher.RearmPage(context.Background(), ""); err != nil {
		t.Fatalf("RearmPage() error = %v", err)
	}
	want := []string{"check", "rearm", "check", "publish", "mark", "activate"}
	if got := events.snapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("rearm events = %v, want %v", got, want)
	}
	if checks.Load() != 2 {
		t.Fatalf("ready checks = %d, want preflight + candidate check", checks.Load())
	}
}

func TestDispatcherRearmReadyPreflightFailureDoesNotTouchSourceOrRabbit(t *testing.T) {
	t.Parallel()

	rearm := &dispatcherRearmFake{rearmFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		t.Fatal("RearmExpiredPage called after failed READY preflight")
		return nil, "", nil
	}}
	rabbit := &dispatcherRabbitFake{}
	coordinator := &dispatcherCoordinatorFake{checkFn: func(context.Context, string, ResourceFence) error {
		return ErrDependencyUnavailable
	}}
	dispatcher := newDispatcherForTest(t, &dispatcherSourceFake{}, rearm, rabbit, coordinator, dispatcherTestOptions())

	if _, err := dispatcher.RearmPage(context.Background(), "cursor"); !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("RearmPage() error = %v, want ErrDependencyUnavailable", err)
	}
	if rearm.calls.Load() != 0 || rabbit.calls.Load() != 0 {
		t.Fatalf("failed preflight touched rearm/Rabbit: %d/%d", rearm.calls.Load(), rabbit.calls.Load())
	}
	if dispatcher.PublisherGateOpen() {
		t.Fatal("failed rearm READY preflight did not close publisher gate")
	}
}

func TestDispatcherAuthoritativeStateCorruptRearmStopsRun(t *testing.T) {
	t.Parallel()

	rearm := &dispatcherRearmFake{rearmFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		return nil, "", ErrAuthoritativeStateCorrupt
	}}
	dispatcher := newDispatcherForTest(t, &dispatcherSourceFake{}, rearm, &dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, dispatcherTestOptions())
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	err := dispatcher.Run(ctx)
	if !errors.Is(err, ErrAuthoritativeStateCorrupt) {
		t.Fatalf("Run() error = %v, want ErrAuthoritativeStateCorrupt", err)
	}
	if dispatcher.PublisherGateOpen() {
		t.Fatal("authoritative rearm corruption left publisher gate open")
	}
	if rearm.calls.Load() != 1 {
		t.Fatalf("RearmExpiredPage calls = %d, want 1", rearm.calls.Load())
	}
}

func TestDispatcherEmptyAdvancingRearmPageContinuesWithoutRoundInterval(t *testing.T) {
	t.Parallel()

	secondPage := make(chan struct{})
	var once sync.Once
	rearm := &dispatcherRearmFake{rearmFn: func(_ context.Context, after string, _ int) ([]DispatchCandidate, string, error) {
		switch after {
		case "":
			return nil, "next-empty-page", nil
		case "next-empty-page":
			once.Do(func() { close(secondPage) })
			return nil, "", nil
		default:
			return nil, "", errors.New("unexpected rearm cursor")
		}
	}}
	options := dispatcherTestOptions()
	options.ExpiredRunningSweepInterval = time.Hour
	options.DispatchInterval = time.Hour
	dispatcher := newDispatcherForTest(t, &dispatcherSourceFake{}, rearm, &dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, options)

	next, err := dispatcher.RearmPage(context.Background(), "")
	if err != nil || next != "next-empty-page" {
		t.Fatalf("RearmPage(empty advancing) = %q, %v", next, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	select {
	case <-secondPage:
		cancel()
	case <-time.After(500 * time.Millisecond):
		cancel()
		t.Fatal("empty advancing page waited for the full sweep interval")
	}
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after cancel")
	}
}

func TestDispatcherEmptyTerminalPageNormalizesRetainedCursor(t *testing.T) {
	t.Parallel()

	const terminal = "terminal-cursor"
	source := &dispatcherSourceFake{listFn: func(_ context.Context, after string, _ int) ([]DispatchCandidate, string, error) {
		return nil, after, nil
	}}
	rearm := &dispatcherRearmFake{rearmFn: func(_ context.Context, after string, _ int) ([]DispatchCandidate, string, error) {
		return nil, after, nil
	}}
	dispatcher := newDispatcherForTest(t, source, rearm, &dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, dispatcherTestOptions())

	next, err := dispatcher.DispatchPage(context.Background(), terminal)
	if err != nil || next != "" {
		t.Fatalf("DispatchPage(retained terminal) = %q, %v, want empty cursor/nil", next, err)
	}
	next, err = dispatcher.RearmPage(context.Background(), terminal)
	if err != nil || next != "" {
		t.Fatalf("RearmPage(retained terminal) = %q, %v, want empty cursor/nil", next, err)
	}
}

func TestDispatcherRearmPageUsesDomainKeysetAndSharedPublisher(t *testing.T) {
	t.Parallel()

	candidates := []DispatchCandidate{
		dispatcherTestCandidate("expired-1", "expired-guard-1", 8),
		dispatcherTestCandidate("expired-2", "expired-guard-2", 9),
	}
	source := &dispatcherSourceFake{markFn: func(_ context.Context, got DispatchCandidate) (bool, error) {
		if got != candidates[0] && got != candidates[1] {
			t.Fatalf("MarkDispatched() got unknown candidate %#v", got)
		}
		return true, nil
	}}
	rearm := &dispatcherRearmFake{rearmFn: func(_ context.Context, after string, limit int) ([]DispatchCandidate, string, error) {
		if after != "expired-cursor" || limit != 2 {
			t.Fatalf("RearmExpiredPage() = after %q limit %d", after, limit)
		}
		return candidates, "expired-next", nil
	}}
	rabbit := &dispatcherRabbitFake{}
	coordinator := &dispatcherCoordinatorFake{}
	dispatcher := newDispatcherForTest(t, source, rearm, rabbit, coordinator, dispatcherTestOptions())

	next, err := dispatcher.RearmPage(context.Background(), "expired-cursor")
	if err != nil {
		t.Fatalf("RearmPage() error = %v", err)
	}
	if next != "expired-next" {
		t.Fatalf("RearmPage() next = %q", next)
	}
	if source.listCalls.Load() != 0 || source.getCalls.Load() != 0 {
		t.Fatalf("generic dispatcher queried outside ExpiredRearmSource: list=%d get=%d", source.listCalls.Load(), source.getCalls.Load())
	}
	if rabbit.calls.Load() != 2 || source.markCalls.Load() != 2 || coordinator.activeCalls.Load() != 2 {
		t.Fatalf("rearm publish/mark/active = %d/%d/%d", rabbit.calls.Load(), source.markCalls.Load(), coordinator.activeCalls.Load())
	}
}

func TestDispatcherRearmCrashAfterArmRecoversThroughOrdinaryScan(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("armed-before-crash", "armed-guard", 10)
	rearm := &dispatcherRearmFake{rearmFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		// The domain transaction committed the new generation, but the source
		// failed before returning it to the reaper.
		return nil, "", errDispatcherTest
	}}
	source := &dispatcherSourceFake{
		listFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
			return []DispatchCandidate{candidate}, "", nil
		},
		markFn: func(_ context.Context, got DispatchCandidate) (bool, error) {
			if !reflect.DeepEqual(got, candidate) {
				t.Fatal("ordinary scan lost rearmed candidate guard")
			}
			return true, nil
		},
	}
	rabbit := &dispatcherRabbitFake{}
	coordinator := &dispatcherCoordinatorFake{}
	dispatcher := newDispatcherForTest(t, source, rearm, rabbit, coordinator, dispatcherTestOptions())

	if _, err := dispatcher.RearmPage(context.Background(), ""); !errors.Is(err, errDispatcherTest) {
		t.Fatalf("RearmPage() error = %v", err)
	}
	if rabbit.calls.Load() != 0 {
		t.Fatal("failed rearm source unexpectedly published")
	}
	if _, err := dispatcher.DispatchPage(context.Background(), ""); err != nil {
		t.Fatalf("DispatchPage() recovery error = %v", err)
	}
	if rabbit.calls.Load() != 1 || source.markCalls.Load() != 1 || coordinator.activeCalls.Load() != 1 {
		t.Fatalf("ordinary recovery publish/mark/active = %d/%d/%d", rabbit.calls.Load(), source.markCalls.Load(), coordinator.activeCalls.Load())
	}
}

func TestDispatcherConcurrentRearmDuplicatesRemainCASIdempotent(t *testing.T) {
	t.Parallel()

	candidate := dispatcherTestCandidate("concurrent-expired", "concurrent-guard", 11)
	ready := make(chan struct{})
	var entered atomic.Int64
	rearm := &dispatcherRearmFake{rearmFn: func(ctx context.Context, _ string, _ int) ([]DispatchCandidate, string, error) {
		if entered.Add(1) == 2 {
			close(ready)
		}
		select {
		case <-ready:
			return []DispatchCandidate{candidate}, "", nil
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}}
	var marked atomic.Bool
	var markWins atomic.Int64
	source := &dispatcherSourceFake{markFn: func(context.Context, DispatchCandidate) (bool, error) {
		if marked.CompareAndSwap(false, true) {
			markWins.Add(1)
			return true, nil
		}
		return false, nil
	}}
	rabbit := &dispatcherRabbitFake{}
	coordinator := &dispatcherCoordinatorFake{}
	dispatcher := newDispatcherForTest(t, source, rearm, rabbit, coordinator, dispatcherTestOptions())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, err := dispatcher.RearmPage(ctx, "")
			errs <- err
		}()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent RearmPage() error = %v", err)
		}
	}
	if markWins.Load() != 1 {
		t.Fatalf("successful MarkDispatched CAS count = %d, want 1", markWins.Load())
	}
	if rabbit.calls.Load() != 2 || coordinator.activeCalls.Load() != 2 {
		t.Fatalf("duplicate publish/active = %d/%d, want safe at-least-once 2/2", rabbit.calls.Load(), coordinator.activeCalls.Load())
	}
}

func TestDispatcherRunBackoffIsBoundedAndCancellationStopsBothLoops(t *testing.T) {
	t.Parallel()

	source := &dispatcherSourceFake{listFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		return nil, "", errDispatcherTest
	}}
	rearm := &dispatcherRearmFake{rearmFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		return nil, "", errDispatcherTest
	}}
	options := dispatcherTestOptions()
	options.DispatchInterval = time.Hour
	options.ExpiredRunningSweepInterval = time.Hour
	options.BackoffInitial = 5 * time.Millisecond
	options.BackoffMax = 10 * time.Millisecond
	dispatcher := newDispatcherForTest(t, source, rearm, &dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, options)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()

	deadline := time.NewTimer(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer deadline.Stop()
	defer ticker.Stop()
	for source.listCalls.Load() < 3 || rearm.calls.Load() < 3 {
		select {
		case <-deadline.C:
			cancel()
			t.Fatalf("bounded retry did not run both loops: dispatch=%d rearm=%d", source.listCalls.Load(), rearm.calls.Load())
		case <-ticker.C:
		}
	}
	if source.listCalls.Load() > 100 || rearm.calls.Load() > 100 {
		cancel()
		t.Fatalf("retry loop busy-spun: dispatch=%d rearm=%d", source.listCalls.Load(), rearm.calls.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run() after cancel error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run() did not stop after context cancellation")
	}
}

func TestDispatcherHealthRecordsOnlySuccessfulDispatchAndSweepPages(t *testing.T) {
	source := &dispatcherSourceFake{}
	rearm := &dispatcherRearmFake{}
	dispatcher := newDispatcherForTest(t, source, rearm, &dispatcherRabbitFake{},
		&dispatcherCoordinatorFake{}, dispatcherTestOptions())
	now := time.Date(2026, 8, 3, 4, 0, 0, 0, time.UTC)
	health := newResourceHealth(func() time.Time { return now })
	health.markGateOpen()
	dispatcher.health = health

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		snapshot := health.snapshot()
		if snapshot.Loops.Dispatcher.LastSuccessAt != nil && snapshot.Loops.Sweeper.LastSuccessAt != nil {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("successful dispatcher loops were not recorded: %+v", snapshot.Loops)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispatcher.Run() error = %v", err)
	}

	failingSource := &dispatcherSourceFake{listFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		return nil, "", errDispatcherTest
	}}
	failingRearm := &dispatcherRearmFake{rearmFn: func(context.Context, string, int) ([]DispatchCandidate, string, error) {
		return nil, "", errDispatcherTest
	}}
	failing := newDispatcherForTest(t, failingSource, failingRearm,
		&dispatcherRabbitFake{}, &dispatcherCoordinatorFake{}, dispatcherTestOptions())
	failingHealth := newResourceHealth(func() time.Time { return now })
	failingHealth.markGateOpen()
	failing.health = failingHealth
	failCtx, failCancel := context.WithCancel(context.Background())
	failDone := make(chan error, 1)
	go func() { failDone <- failing.Run(failCtx) }()
	deadline = time.Now().Add(time.Second)
	for failingSource.listCalls.Load() < 2 || failingRearm.calls.Load() < 2 {
		if time.Now().After(deadline) {
			failCancel()
			t.Fatalf("failed dispatcher loops did not run: dispatch=%d sweep=%d",
				failingSource.listCalls.Load(), failingRearm.calls.Load())
		}
		time.Sleep(time.Millisecond)
	}
	failCancel()
	<-failDone
	failedSnapshot := failingHealth.snapshot()
	if failedSnapshot.Loops.Dispatcher.LastSuccessAt != nil || failedSnapshot.Loops.Sweeper.LastSuccessAt != nil {
		t.Fatalf("failed dispatcher iteration invented success: %+v", failedSnapshot.Loops)
	}
}
