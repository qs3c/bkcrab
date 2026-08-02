package fairqueue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	rabbitTestAttemptA = "0000000000000000000000000000000a"
	rabbitTestAttemptB = "0000000000000000000000000000000b"
	rabbitTestAttemptC = "0000000000000000000000000000000c"
)

type rabbitTestDialResult struct {
	connection rabbitConnection
	err        error
}

type rabbitTestDialer struct {
	mu      sync.Mutex
	results []rabbitTestDialResult
	calls   int
}

func (d *rabbitTestDialer) Dial(context.Context, string) (rabbitConnection, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if len(d.results) == 0 {
		return nil, errors.New("unexpected RabbitMQ dial")
	}
	result := d.results[0]
	d.results = d.results[1:]
	return result.connection, result.err
}

func (d *rabbitTestDialer) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

type rabbitTestConnection struct {
	channel      *rabbitTestChannel
	openErr      error
	openGate     <-chan struct{}
	closeErr     error
	aborted      chan struct{}
	abortOnce    sync.Once
	closeCalls   atomic.Int32
	abortCalls   atomic.Int32
	deadlineMu   sync.Mutex
	lastDeadline time.Time
}

func (c *rabbitTestConnection) OpenChannel() (rabbitChannel, error) {
	if c.openGate != nil {
		select {
		case <-c.openGate:
		case <-c.aborted:
			return nil, errors.New("fake connection aborted while opening channel")
		}
	}
	if c.openErr != nil {
		return nil, c.openErr
	}
	return c.channel, nil
}

func (c *rabbitTestConnection) CloseDeadline(deadline time.Time) error {
	c.closeCalls.Add(1)
	c.deadlineMu.Lock()
	c.lastDeadline = deadline
	c.deadlineMu.Unlock()
	if c.closeErr != nil {
		return c.closeErr
	}
	c.channel.closeLocal()
	return nil
}

func (c *rabbitTestConnection) Abort() error {
	c.abortCalls.Add(1)
	c.abortOnce.Do(func() {
		if c.aborted != nil {
			close(c.aborted)
		}
		c.channel.closeLocal()
	})
	return nil
}

type rabbitTestOpKind string

const (
	rabbitTestExchangeDeclare rabbitTestOpKind = "exchange-declare"
	rabbitTestQueueDeclare    rabbitTestOpKind = "queue-declare"
	rabbitTestQueueBind       rabbitTestOpKind = "queue-bind"
	rabbitTestQueueInspect    rabbitTestOpKind = "queue-inspect"
	rabbitTestConfirm         rabbitTestOpKind = "confirm"
	rabbitTestPublish         rabbitTestOpKind = "publish"
	rabbitTestGet             rabbitTestOpKind = "get"
)

type rabbitTestOp struct {
	kind       rabbitTestOpKind
	name       string
	exchange   string
	routingKey string
	durable    bool
	autoDelete bool
	exclusive  bool
	mandatory  bool
	immediate  bool
	args       amqp.Table
	publishing amqp.Publishing
}

type rabbitTestPublishCall struct {
	sequence   uint64
	exchange   string
	routingKey string
	mandatory  bool
	immediate  bool
	publishing amqp.Publishing
}

type rabbitTestGetResult struct {
	delivery amqp.Delivery
	ok       bool
	err      error
}

type rabbitTestChannel struct {
	mu sync.Mutex

	nextSequence uint64
	ops          []rabbitTestOp
	getResults   []rabbitTestGetResult
	inspect      amqp.Queue
	publishErr   error
	blockPublish bool
	publishGate  <-chan struct{}
	rpcGates     map[rabbitTestOpKind]<-chan struct{}
	rpcErrors    map[rabbitTestOpKind]error
	rpcStarted   chan rabbitTestOpKind

	confirms  chan amqp.Confirmation
	returns   chan amqp.Return
	closes    chan *amqp.Error
	published chan rabbitTestPublishCall
	closed    chan struct{}
	closeOnce sync.Once
}

func newRabbitTestChannel() *rabbitTestChannel {
	return &rabbitTestChannel{
		nextSequence: 1,
		published:    make(chan rabbitTestPublishCall, 8),
		closed:       make(chan struct{}),
		rpcGates:     make(map[rabbitTestOpKind]<-chan struct{}),
		rpcErrors:    make(map[rabbitTestOpKind]error),
		rpcStarted:   make(chan rabbitTestOpKind, 8),
	}
}

func (c *rabbitTestChannel) record(operation rabbitTestOp) {
	c.mu.Lock()
	c.ops = append(c.ops, operation)
	c.mu.Unlock()
}

func (c *rabbitTestChannel) ExchangeDeclare(name, kind string, durable, autoDelete, internal, noWait bool, args amqp.Table) error {
	c.record(rabbitTestOp{kind: rabbitTestExchangeDeclare, name: name, routingKey: kind, durable: durable, autoDelete: autoDelete, args: cloneRabbitTestTable(args)})
	return c.finishRPC(rabbitTestExchangeDeclare)
}

func (c *rabbitTestChannel) QueueDeclare(name string, durable, autoDelete, exclusive, noWait bool, args amqp.Table) (amqp.Queue, error) {
	c.record(rabbitTestOp{kind: rabbitTestQueueDeclare, name: name, durable: durable, autoDelete: autoDelete, exclusive: exclusive, args: cloneRabbitTestTable(args)})
	return amqp.Queue{Name: name}, c.finishRPC(rabbitTestQueueDeclare)
}

func (c *rabbitTestChannel) QueueBind(name, key, exchange string, noWait bool, args amqp.Table) error {
	c.record(rabbitTestOp{kind: rabbitTestQueueBind, name: name, exchange: exchange, routingKey: key, args: cloneRabbitTestTable(args)})
	return c.finishRPC(rabbitTestQueueBind)
}

func (c *rabbitTestChannel) QueueInspect(name string) (amqp.Queue, error) {
	c.record(rabbitTestOp{kind: rabbitTestQueueInspect, name: name})
	if err := c.finishRPC(rabbitTestQueueInspect); err != nil {
		return amqp.Queue{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inspect, nil
}

func (c *rabbitTestChannel) Confirm(bool) error {
	c.record(rabbitTestOp{kind: rabbitTestConfirm})
	return c.finishRPC(rabbitTestConfirm)
}

func (c *rabbitTestChannel) NotifyPublish(confirm chan amqp.Confirmation) chan amqp.Confirmation {
	c.mu.Lock()
	c.confirms = confirm
	c.mu.Unlock()
	return confirm
}

func (c *rabbitTestChannel) NotifyReturn(returned chan amqp.Return) chan amqp.Return {
	c.mu.Lock()
	c.returns = returned
	c.mu.Unlock()
	return returned
}

func (c *rabbitTestChannel) NotifyClose(closed chan *amqp.Error) chan *amqp.Error {
	c.mu.Lock()
	c.closes = closed
	c.mu.Unlock()
	return closed
}

func (c *rabbitTestChannel) GetNextPublishSeqNo() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nextSequence
}

func (c *rabbitTestChannel) PublishWithContext(_ context.Context, exchange, routingKey string, mandatory, immediate bool, publishing amqp.Publishing) error {
	c.mu.Lock()
	sequence := c.nextSequence
	c.nextSequence++
	call := rabbitTestPublishCall{
		sequence: sequence, exchange: exchange, routingKey: routingKey,
		mandatory: mandatory, immediate: immediate, publishing: cloneRabbitTestPublishing(publishing),
	}
	c.ops = append(c.ops, rabbitTestOp{
		kind: rabbitTestPublish, exchange: exchange, routingKey: routingKey,
		mandatory: mandatory, immediate: immediate, publishing: cloneRabbitTestPublishing(publishing),
	})
	publishErr := c.publishErr
	blocked := c.blockPublish
	publishGate := c.publishGate
	c.mu.Unlock()
	c.published <- call
	if publishGate != nil {
		select {
		case <-publishGate:
		case <-c.closed:
			return errors.New("fake channel closed before publish gate opened")
		}
	}
	if blocked {
		<-c.closed
		return errors.New("fake channel closed while publish was blocked")
	}
	return publishErr
}

func (c *rabbitTestChannel) Get(queue string, autoAck bool) (amqp.Delivery, bool, error) {
	c.record(rabbitTestOp{kind: rabbitTestGet, name: queue, immediate: autoAck})
	if err := c.finishRPC(rabbitTestGet); err != nil {
		return amqp.Delivery{}, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.getResults) == 0 {
		return amqp.Delivery{}, false, nil
	}
	result := c.getResults[0]
	c.getResults = c.getResults[1:]
	return result.delivery, result.ok, result.err
}

func (c *rabbitTestChannel) Close() error {
	c.closeLocal()
	return nil
}

func (c *rabbitTestChannel) closeLocal() {
	c.closeOnce.Do(func() { close(c.closed) })
}

func (c *rabbitTestChannel) finishRPC(kind rabbitTestOpKind) error {
	c.mu.Lock()
	gate := c.rpcGates[kind]
	err := c.rpcErrors[kind]
	started := c.rpcStarted
	c.mu.Unlock()
	select {
	case started <- kind:
	default:
	}
	if gate != nil {
		select {
		case <-gate:
		case <-c.closed:
			return errors.New("fake channel closed during RPC")
		}
	}
	return err
}

func (c *rabbitTestChannel) waitRPC(t *testing.T, want rabbitTestOpKind) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case got := <-c.rpcStarted:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s RPC", want)
		}
	}
}

func (c *rabbitTestChannel) enqueueGet(result rabbitTestGetResult) {
	c.mu.Lock()
	c.getResults = append(c.getResults, result)
	c.mu.Unlock()
}

func (c *rabbitTestChannel) takePublish(t *testing.T) rabbitTestPublishCall {
	t.Helper()
	select {
	case call := <-c.published:
		return call
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for fake RabbitMQ publish")
		return rabbitTestPublishCall{}
	}
}

func (c *rabbitTestChannel) emitConfirm(t *testing.T, call rabbitTestPublishCall, ack bool) {
	t.Helper()
	c.mu.Lock()
	confirmations := c.confirms
	c.mu.Unlock()
	if confirmations == nil {
		t.Fatal("publisher confirmation listener was not registered")
	}
	confirmations <- amqp.Confirmation{DeliveryTag: call.sequence, Ack: ack}
}

func (c *rabbitTestChannel) emitReturn(t *testing.T, call rabbitTestPublishCall) {
	t.Helper()
	c.mu.Lock()
	returns := c.returns
	c.mu.Unlock()
	if returns == nil {
		t.Fatal("mandatory return listener was not registered")
	}
	returns <- amqp.Return{
		MessageId: call.publishing.MessageId, CorrelationId: call.publishing.CorrelationId,
		Exchange: call.exchange, RoutingKey: call.routingKey,
	}
}

func (c *rabbitTestChannel) emitClose(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	closes := c.closes
	c.mu.Unlock()
	if closes == nil {
		t.Fatal("channel close listener was not registered")
	}
	closes <- &amqp.Error{Code: 504, Reason: "fake channel closed"}
}

func (c *rabbitTestChannel) snapshotOps() []rabbitTestOp {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]rabbitTestOp(nil), c.ops...)
}

func cloneRabbitTestPublishing(publishing amqp.Publishing) amqp.Publishing {
	clone := publishing
	clone.Headers = cloneRabbitTestTable(publishing.Headers)
	clone.Body = append([]byte(nil), publishing.Body...)
	return clone
}

func cloneRabbitTestTable(table amqp.Table) amqp.Table {
	if table == nil {
		return nil
	}
	clone := make(amqp.Table, len(table))
	for key, value := range table {
		clone[key] = value
	}
	return clone
}

type rabbitTestAttemptIDs struct {
	mu  sync.Mutex
	ids []string
}

func (s *rabbitTestAttemptIDs) Next() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.ids) == 0 {
		return "", errors.New("unexpected attempt allocation")
	}
	id := s.ids[0]
	s.ids = s.ids[1:]
	return id, nil
}

type rabbitTestSettlement struct {
	kind     string
	tag      uint64
	multiple bool
	requeue  bool
}

type rabbitTestAcknowledger struct {
	mu      sync.Mutex
	calls   []rabbitTestSettlement
	err     error
	gate    <-chan struct{}
	started chan struct{}
}

func (a *rabbitTestAcknowledger) Ack(tag uint64, multiple bool) error {
	a.mu.Lock()
	a.calls = append(a.calls, rabbitTestSettlement{kind: "ack", tag: tag, multiple: multiple})
	err := a.err
	gate := a.gate
	started := a.started
	a.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	return err
}

func (a *rabbitTestAcknowledger) Nack(tag uint64, multiple, requeue bool) error {
	a.mu.Lock()
	a.calls = append(a.calls, rabbitTestSettlement{kind: "nack", tag: tag, multiple: multiple, requeue: requeue})
	err := a.err
	gate := a.gate
	started := a.started
	a.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if gate != nil {
		<-gate
	}
	return err
}

func (a *rabbitTestAcknowledger) Reject(tag uint64, requeue bool) error {
	a.mu.Lock()
	a.calls = append(a.calls, rabbitTestSettlement{kind: "reject", tag: tag, requeue: requeue})
	a.mu.Unlock()
	return nil
}

func (a *rabbitTestAcknowledger) snapshot() []rabbitTestSettlement {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]rabbitTestSettlement(nil), a.calls...)
}

type rabbitTestPublishResult struct {
	receipt PublishReceipt
	err     error
}

func rabbitTestConfig() ResourceConfig {
	return ResourceConfig{
		Key:                         "rag.index",
		ValidateTaskID:              ValidateRAGIndexTaskID,
		LocalWorkers:                2,
		GlobalConcurrency:           4,
		PerUserBaseConcurrency:      2,
		PerUserBurstConcurrency:     4,
		BorrowEnabled:               true,
		ReconcileInterval:           time.Second,
		ExpiredRunningSweepInterval: time.Second,
		ReconcilePageSize:           10,
		ReservationTTL:              time.Minute,
		ReservationHeartbeat:        20 * time.Second,
		PrepareTimeout:              5 * time.Second,
		ProvisionalTTL:              10 * time.Second,
		ProcessingTurnTTL:           10 * time.Second,
		RecoveryDrainTimeout:        2 * time.Minute,
		DispatchInterval:            time.Second,
		PublishAttemptTimeout:       15 * time.Second,
	}
}

func newRabbitTestClient(t *testing.T, dialer *rabbitTestDialer, ids ...string) *Rabbit {
	return newRabbitTestClientWithOptions(t, dialer, RabbitOptions{
		URL: "amqp://unit.test/", Exchange: "test.fair.task", DeadLetterExchange: "test.fair.dlx",
	}, ids...)
}

func newRabbitTestClientWithOptions(t *testing.T, dialer *rabbitTestDialer, options RabbitOptions, ids ...string) *Rabbit {
	t.Helper()
	registry, err := NewRegistry(rabbitTestConfig())
	if err != nil {
		t.Fatalf("NewRegistry() error = %v", err)
	}
	client, err := newRabbit(options, registry, dialer, &rabbitTestAttemptIDs{ids: append([]string(nil), ids...)})
	if err != nil {
		t.Fatalf("newRabbit() error = %v", err)
	}
	return client
}

func rabbitTestMessage() Message {
	return Message{
		Version: MessageVersion1, Resource: "rag.index", TenantID: "tenant-a", TaskType: "rag.index", TaskID: "42",
		DispatchToken: DispatchToken{Resource: "rag.index", TaskID: "42", Generation: 7},
	}
}

func rabbitTestDelivery(t *testing.T, message Message, attempt string, acknowledger amqp.Acknowledger) amqp.Delivery {
	t.Helper()
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal test message: %v", err)
	}
	return amqp.Delivery{
		Acknowledger: acknowledger,
		Headers: amqp.Table{
			rabbitHeaderProtocolVersion: int32(MessageVersion1), rabbitHeaderResource: message.Resource,
			rabbitHeaderTaskID: message.TaskID, rabbitHeaderDispatchGeneration: int64(message.DispatchToken.Generation),
		},
		DeliveryMode: amqp.Persistent, CorrelationId: attempt, MessageId: attempt,
		Body: body, DeliveryTag: 19,
	}
}

func rabbitTestPublishAsync(client *Rabbit, ctx context.Context, message Message) <-chan rabbitTestPublishResult {
	result := make(chan rabbitTestPublishResult, 1)
	go func() {
		receipt, err := client.PublishMandatoryConfirmed(ctx, message)
		result <- rabbitTestPublishResult{receipt: receipt, err: err}
	}()
	return result
}

func rabbitTestDeadLetterAsync(client *Rabbit, ctx context.Context, request DeadLetterRequest) <-chan rabbitTestPublishResult {
	result := make(chan rabbitTestPublishResult, 1)
	go func() {
		receipt, err := client.PublishDeadLetterConfirmed(ctx, request)
		result <- rabbitTestPublishResult{receipt: receipt, err: err}
	}()
	return result
}

func rabbitTestAwaitPublishResult(t *testing.T, result <-chan rabbitTestPublishResult) rabbitTestPublishResult {
	t.Helper()
	select {
	case outcome := <-result:
		return outcome
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for RabbitMQ publish result")
		return rabbitTestPublishResult{}
	}
}

func TestRabbitPublishDeclaresTopologyAndWritesCanonicalEnvelope(t *testing.T) {
	channel := newRabbitTestChannel()
	connection := &rabbitTestConnection{channel: channel}
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
	client := newRabbitTestClient(t, dialer, rabbitTestAttemptA, rabbitTestAttemptB)
	message := rabbitTestMessage()

	firstResult := rabbitTestPublishAsync(client, context.Background(), message)
	first := channel.takePublish(t)
	channel.emitConfirm(t, first, true)
	outcome := rabbitTestAwaitPublishResult(t, firstResult)
	if outcome.err != nil || outcome.receipt.AttemptID != rabbitTestAttemptA {
		t.Fatalf("first publish = %+v, want successful receipt %q", outcome, rabbitTestAttemptA)
	}
	if first.exchange != "test.fair.task" || !first.mandatory || first.immediate {
		t.Fatalf("publish routing/options = %+v", first)
	}
	wantRouting, _ := TenantRoutingKey(message.Resource, message.TenantID)
	if first.routingKey != wantRouting {
		t.Fatalf("routing key = %q, want %q", first.routingKey, wantRouting)
	}
	publishing := first.publishing
	if publishing.DeliveryMode != amqp.Persistent || publishing.ContentType != "application/json" ||
		publishing.MessageId != rabbitTestAttemptA || publishing.CorrelationId != rabbitTestAttemptA {
		t.Fatalf("canonical AMQP properties = %+v", publishing)
	}
	if _, ok := publishing.Headers[rabbitHeaderProtocolVersion].(int32); !ok {
		t.Fatalf("protocol header type = %T, want int32", publishing.Headers[rabbitHeaderProtocolVersion])
	}
	if _, ok := publishing.Headers[rabbitHeaderResource].(string); !ok {
		t.Fatalf("resource header type = %T, want string", publishing.Headers[rabbitHeaderResource])
	}
	if _, ok := publishing.Headers[rabbitHeaderTaskID].(string); !ok {
		t.Fatalf("task header type = %T, want string", publishing.Headers[rabbitHeaderTaskID])
	}
	if _, ok := publishing.Headers[rabbitHeaderDispatchGeneration].(int64); !ok {
		t.Fatalf("generation header type = %T, want int64", publishing.Headers[rabbitHeaderDispatchGeneration])
	}
	decoded, err := StrictDecodeMessage(publishing.Body)
	if err != nil || decoded != message {
		t.Fatalf("published body = %+v, %v", decoded, err)
	}

	opsAfterFirst := channel.snapshotOps()
	assertRabbitTestTopology(t, opsAfterFirst, message)
	declarations := rabbitTestDeclarationCount(opsAfterFirst)

	secondResult := rabbitTestPublishAsync(client, context.Background(), message)
	second := channel.takePublish(t)
	channel.emitConfirm(t, second, true)
	if outcome := rabbitTestAwaitPublishResult(t, secondResult); outcome.err != nil || outcome.receipt.AttemptID != rabbitTestAttemptB {
		t.Fatalf("second publish = %+v", outcome)
	}
	if got := rabbitTestDeclarationCount(channel.snapshotOps()); got != declarations {
		t.Fatalf("same-generation topology declarations = %d, want cached %d", got, declarations)
	}
}

func TestRabbitPublishOutcomesReturnReceipt(t *testing.T) {
	tests := []struct {
		name       string
		drive      func(*testing.T, *rabbitTestChannel, rabbitTestPublishCall, context.CancelFunc)
		wantError  error
		block      bool
		publishErr error
	}{
		{name: "confirm ack", drive: func(t *testing.T, channel *rabbitTestChannel, call rabbitTestPublishCall, _ context.CancelFunc) {
			channel.emitConfirm(t, call, true)
		}},
		{name: "confirm nack", drive: func(t *testing.T, channel *rabbitTestChannel, call rabbitTestPublishCall, _ context.CancelFunc) {
			channel.emitConfirm(t, call, false)
		}, wantError: ErrPublishUnconfirmed},
		{name: "return then positive confirm", drive: func(t *testing.T, channel *rabbitTestChannel, call rabbitTestPublishCall, _ context.CancelFunc) {
			channel.emitReturn(t, call)
			channel.emitConfirm(t, call, true)
		}, wantError: ErrPublishUnroutable},
		{name: "deadline while publish blocks", drive: func(_ *testing.T, _ *rabbitTestChannel, _ rabbitTestPublishCall, cancel context.CancelFunc) { cancel() }, wantError: ErrPublishUnconfirmed, block: true},
		{name: "publish call error", drive: func(_ *testing.T, _ *rabbitTestChannel, _ rabbitTestPublishCall, _ context.CancelFunc) {}, wantError: ErrPublishUnconfirmed, publishErr: errors.New("publish failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := newRabbitTestChannel()
			channel.blockPublish = test.block
			channel.publishErr = test.publishErr
			connection := &rabbitTestConnection{channel: channel}
			dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
			client := newRabbitTestClient(t, dialer, rabbitTestAttemptA)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := rabbitTestPublishAsync(client, ctx, rabbitTestMessage())
			call := channel.takePublish(t)
			test.drive(t, channel, call, cancel)
			outcome := rabbitTestAwaitPublishResult(t, result)
			if outcome.receipt.AttemptID != rabbitTestAttemptA {
				t.Fatalf("receipt = %+v, want allocated attempt", outcome.receipt)
			}
			if test.wantError == nil && outcome.err != nil {
				t.Fatalf("publish error = %v", outcome.err)
			}
			if test.wantError != nil && !errors.Is(outcome.err, test.wantError) {
				t.Fatalf("publish error = %v, want errors.Is(%v)", outcome.err, test.wantError)
			}
		})
	}
}

func TestRabbitPublishConfirmationDeadlineAbortsGeneration(t *testing.T) {
	channel := newRabbitTestChannel()
	connection := &rabbitTestConnection{channel: channel}
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
	client := newRabbitTestClientWithOptions(t, dialer, RabbitOptions{
		URL: "amqp://unit.test/", OperationTimeout: 30 * time.Millisecond,
	}, rabbitTestAttemptA)

	result := rabbitTestPublishAsync(client, context.Background(), rabbitTestMessage())
	_ = channel.takePublish(t)
	outcome := rabbitTestAwaitPublishResult(t, result)
	if outcome.receipt.AttemptID != rabbitTestAttemptA ||
		!errors.Is(outcome.err, ErrPublishUnconfirmed) ||
		!errors.Is(outcome.err, ErrDependencyUnavailable) ||
		!errors.Is(outcome.err, context.DeadlineExceeded) {
		t.Fatalf("confirmation deadline outcome = %+v", outcome)
	}
	if connection.abortCalls.Load() != 1 {
		t.Fatalf("confirmation deadline Abort calls = %d, want 1", connection.abortCalls.Load())
	}
}

func TestRabbitReturnAndNackPreserveBothFailureCategories(t *testing.T) {
	channel := newRabbitTestChannel()
	connection := &rabbitTestConnection{channel: channel}
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
	client := newRabbitTestClient(t, dialer, rabbitTestAttemptA)

	result := rabbitTestPublishAsync(client, context.Background(), rabbitTestMessage())
	call := channel.takePublish(t)
	channel.emitReturn(t, call)
	channel.emitConfirm(t, call, false)
	outcome := rabbitTestAwaitPublishResult(t, result)
	if outcome.receipt.AttemptID != rabbitTestAttemptA ||
		!errors.Is(outcome.err, ErrPublishUnconfirmed) ||
		!errors.Is(outcome.err, ErrPublishUnroutable) {
		t.Fatalf("return plus NACK outcome = %+v", outcome)
	}
}

func TestRabbitPublishDrainsReturnWhenConfirmationIsObservedFirst(t *testing.T) {
	// Both notifications are queued before PublishWithContext returns. This
	// deterministically exercises the application-level observation race: the
	// confirm may be selected first even though Rabbit notified the return in
	// the same publish outcome window.
	publishGate := make(chan struct{})
	channel := newRabbitTestChannel()
	channel.publishGate = publishGate
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: &rabbitTestConnection{channel: channel}}}}
	client := newRabbitTestClient(t, dialer, rabbitTestAttemptA)

	result := rabbitTestPublishAsync(client, context.Background(), rabbitTestMessage())
	call := channel.takePublish(t)
	channel.emitConfirm(t, call, true)
	channel.emitReturn(t, call)
	close(publishGate)
	outcome := rabbitTestAwaitPublishResult(t, result)
	if !errors.Is(outcome.err, ErrPublishUnroutable) || outcome.receipt.AttemptID != rabbitTestAttemptA {
		t.Fatalf("confirm/return observation race = %+v", outcome)
	}
}

func TestRabbitReturnInvalidatesTenantTopologyCache(t *testing.T) {
	channel := newRabbitTestChannel()
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: &rabbitTestConnection{channel: channel}}}}
	client := newRabbitTestClient(t, dialer, rabbitTestAttemptA, rabbitTestAttemptB)

	firstResult := rabbitTestPublishAsync(client, context.Background(), rabbitTestMessage())
	first := channel.takePublish(t)
	channel.emitReturn(t, first)
	channel.emitConfirm(t, first, true)
	if outcome := rabbitTestAwaitPublishResult(t, firstResult); !errors.Is(outcome.err, ErrPublishUnroutable) {
		t.Fatalf("unroutable publish = %+v", outcome)
	}
	declarationsAfterReturn := rabbitTestDeclarationCount(channel.snapshotOps())

	secondResult := rabbitTestPublishAsync(client, context.Background(), rabbitTestMessage())
	second := channel.takePublish(t)
	channel.emitConfirm(t, second, true)
	if outcome := rabbitTestAwaitPublishResult(t, secondResult); outcome.err != nil {
		t.Fatalf("publish after topology repair = %+v", outcome)
	}
	// Base exchanges and the resource DLQ remain cached, but the tenant queue
	// and binding must be declared again after a mandatory return.
	if got := rabbitTestDeclarationCount(channel.snapshotOps()); got != declarationsAfterReturn+2 {
		t.Fatalf("declarations after return = %d, want %d", got, declarationsAfterReturn+2)
	}
}

func TestRabbitReconnectClearsTopologyCacheAndIgnoresOldGeneration(t *testing.T) {
	firstChannel := newRabbitTestChannel()
	secondChannel := newRabbitTestChannel()
	firstConnection := &rabbitTestConnection{channel: firstChannel}
	secondConnection := &rabbitTestConnection{channel: secondChannel}
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: firstConnection}, {connection: secondConnection}}}
	client := newRabbitTestClient(t, dialer, rabbitTestAttemptA, rabbitTestAttemptB)

	firstResult := rabbitTestPublishAsync(client, context.Background(), rabbitTestMessage())
	firstCall := firstChannel.takePublish(t)
	firstChannel.emitClose(t)
	if outcome := rabbitTestAwaitPublishResult(t, firstResult); !errors.Is(outcome.err, ErrPublishUnconfirmed) {
		t.Fatalf("closed-generation publish error = %v", outcome.err)
	}

	secondResult := rabbitTestPublishAsync(client, context.Background(), rabbitTestMessage())
	secondCall := secondChannel.takePublish(t)
	// A late old-generation event must not complete the new sequence-1 attempt.
	firstChannel.emitReturn(t, firstCall)
	firstChannel.emitConfirm(t, firstCall, true)
	secondChannel.emitConfirm(t, secondCall, true)
	if outcome := rabbitTestAwaitPublishResult(t, secondResult); outcome.err != nil || outcome.receipt.AttemptID != rabbitTestAttemptB {
		t.Fatalf("reconnected publish = %+v", outcome)
	}
	if dialer.callCount() != 2 {
		t.Fatalf("dial count = %d, want 2", dialer.callCount())
	}
	if rabbitTestDeclarationCount(firstChannel.snapshotOps()) == 0 || rabbitTestDeclarationCount(secondChannel.snapshotOps()) == 0 {
		t.Fatal("topology was not redeclared in both connection generations")
	}
}

func TestRabbitGetOnePreservesIndependentFacts(t *testing.T) {
	base := rabbitTestMessage()
	tests := []struct {
		name          string
		mutate        func(*amqp.Delivery)
		wantMessage   bool
		wantBody      bool
		wantHeader    bool
		wantOmitted   bool
		wantHeaderErr bool
		wantPropErr   bool
	}{
		{name: "canonical", wantMessage: true, wantBody: true, wantHeader: true},
		{name: "bad body retains header", mutate: func(delivery *amqp.Delivery) { delivery.Body = []byte("{bad") }, wantHeader: true},
		{name: "bad protocol retains repair token", mutate: func(delivery *amqp.Delivery) { delivery.Headers[rabbitHeaderProtocolVersion] = int64(1) }, wantBody: true, wantHeader: true, wantHeaderErr: true},
		{name: "bad task header type preserves partial facts", mutate: func(delivery *amqp.Delivery) { delivery.Headers[rabbitHeaderTaskID] = []byte("42") }, wantBody: true, wantHeaderErr: true},
		{name: "two valid mismatched locators", mutate: func(delivery *amqp.Delivery) { delivery.Headers[rabbitHeaderTaskID] = "43" }, wantBody: true, wantHeader: true},
		{name: "bad properties retain locators", mutate: func(delivery *amqp.Delivery) { delivery.CorrelationId = rabbitTestAttemptB }, wantBody: true, wantHeader: true, wantPropErr: true},
		{name: "protocol-sized overflow retains raw only", mutate: func(delivery *amqp.Delivery) { delivery.Body = bytes.Repeat([]byte{'x'}, MaxMessageBytes+1) }, wantHeader: true},
		{name: "raw overflow keeps digest facts", mutate: func(delivery *amqp.Delivery) { delivery.Body = bytes.Repeat([]byte{'z'}, MaxRawDeliveryBytes+1) }, wantHeader: true, wantOmitted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := newRabbitTestChannel()
			acknowledger := &rabbitTestAcknowledger{}
			delivery := rabbitTestDelivery(t, base, rabbitTestAttemptA, acknowledger)
			if test.mutate != nil {
				test.mutate(&delivery)
			}
			originalBody := append([]byte(nil), delivery.Body...)
			channel.enqueueGet(rabbitTestGetResult{delivery: delivery, ok: true})
			dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: &rabbitTestConnection{channel: channel}}}}
			client := newRabbitTestClient(t, dialer)
			got, ok, err := client.GetOne(context.Background(), base.Resource, base.TenantID)
			if err != nil || !ok {
				t.Fatalf("GetOne() = %v, %v, want delivery: %v", got, ok, err)
			}
			request := got.Request()
			if err := request.Validate(); err != nil {
				t.Fatalf("PrepareRequest.Validate() = %v; request=%+v", err, request)
			}
			if (request.Message != nil) != test.wantMessage || (request.BodyCandidate != nil) != test.wantBody ||
				(request.HeaderToken != nil) != test.wantHeader || request.RawBodyOmitted != test.wantOmitted {
				t.Fatalf("independent facts = message:%t body:%t header:%t omitted:%t; request=%+v",
					request.Message != nil, request.BodyCandidate != nil, request.HeaderToken != nil, request.RawBodyOmitted, request)
			}
			if (request.HeaderErrorCode != "") != test.wantHeaderErr || (request.PropertyErrorCode != "") != test.wantPropErr {
				t.Fatalf("error facts = header:%q property:%q", request.HeaderErrorCode, request.PropertyErrorCode)
			}
			if test.wantOmitted {
				digest := sha256.Sum256(originalBody)
				if len(request.RawBody) != 0 || request.RawBodySize != int64(len(originalBody)) || request.RawBodySHA256 != hex.EncodeToString(digest[:]) {
					t.Fatalf("omitted body facts = len:%d size:%d hash:%q", len(request.RawBody), request.RawBodySize, request.RawBodySHA256)
				}
			} else if !bytes.Equal(request.RawBody, originalBody) {
				t.Fatal("retained raw body changed")
			}
			if len(request.RawBody) != 0 {
				request.RawBody[0] ^= 0xff
				if bytes.Equal(request.RawBody, got.Request().RawBody) {
					t.Fatal("Delivery.Request returned mutable shared raw body")
				}
			}
		})
	}
}

func TestRabbitDeliverySettlesExactlyOnce(t *testing.T) {
	t.Run("ack", func(t *testing.T) {
		acknowledger := &rabbitTestAcknowledger{}
		delivery := &rabbitDelivery{acknowledger: acknowledger, deliveryTag: 7}
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("Ack() error = %v", err)
		}
		if err := delivery.Nack(context.Background(), true); !errors.Is(err, ErrDeliverySettled) {
			t.Fatalf("second settlement error = %v", err)
		}
		calls := acknowledger.snapshot()
		if len(calls) != 1 || calls[0] != (rabbitTestSettlement{kind: "ack", tag: 7}) {
			t.Fatalf("broker settlements = %+v", calls)
		}
	})

	t.Run("nack requeue", func(t *testing.T) {
		acknowledger := &rabbitTestAcknowledger{}
		delivery := &rabbitDelivery{acknowledger: acknowledger, deliveryTag: 8}
		if err := delivery.Nack(context.Background(), true); err != nil {
			t.Fatalf("Nack() error = %v", err)
		}
		calls := acknowledger.snapshot()
		if len(calls) != 1 || calls[0] != (rabbitTestSettlement{kind: "nack", tag: 8, requeue: true}) {
			t.Fatalf("broker settlements = %+v", calls)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		acknowledger := &rabbitTestAcknowledger{}
		delivery := &rabbitDelivery{acknowledger: acknowledger, deliveryTag: 9}
		start := make(chan struct{})
		results := make(chan error, 2)
		go func() { <-start; results <- delivery.Ack(context.Background()) }()
		go func() { <-start; results <- delivery.Nack(context.Background(), true) }()
		close(start)
		first, second := <-results, <-results
		if (first == nil) == (second == nil) || (!errors.Is(first, ErrDeliverySettled) && !errors.Is(second, ErrDeliverySettled)) {
			t.Fatalf("concurrent settlement errors = %v, %v", first, second)
		}
		if calls := acknowledger.snapshot(); len(calls) != 1 {
			t.Fatalf("broker settlements = %+v, want exactly one", calls)
		}
	})
}

func TestRabbitDeadLetterEnvelope(t *testing.T) {
	tests := []struct {
		name    string
		body    []byte
		omitted bool
	}{
		{name: "retained", body: []byte("{not-json")},
		{name: "omitted", body: bytes.Repeat([]byte{'q'}, MaxRawDeliveryBytes+1), omitted: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := newRabbitTestChannel()
			dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: &rabbitTestConnection{channel: channel}}}}
			client := newRabbitTestClient(t, dialer, rabbitTestAttemptB)
			message := rabbitTestMessage()
			delivery := rabbitTestDelivery(t, message, rabbitTestAttemptA, &rabbitTestAcknowledger{})
			delivery.Body = append([]byte(nil), test.body...)
			topology, err := client.validateTenantTopology(message.Resource, message.TenantID)
			if err != nil {
				t.Fatalf("validateTenantTopology() error = %v", err)
			}
			request := DeadLetterRequest{Delivery: client.prepareRequest(delivery, topology), ReasonCode: "poison-message"}
			if err := request.Validate(); err != nil {
				t.Fatalf("DeadLetterRequest.Validate() error = %v", err)
			}

			result := rabbitTestDeadLetterAsync(client, context.Background(), request)
			call := channel.takePublish(t)
			channel.emitConfirm(t, call, true)
			outcome := rabbitTestAwaitPublishResult(t, result)
			if outcome.err != nil || outcome.receipt.AttemptID != rabbitTestAttemptB {
				t.Fatalf("DLQ publish = %+v", outcome)
			}
			if call.exchange != "test.fair.dlx" || call.routingKey != message.Resource || !call.mandatory || call.publishing.DeliveryMode != amqp.Persistent {
				t.Fatalf("DLQ routing/properties = %+v", call)
			}
			if call.publishing.Headers[rabbitDLQHeaderReason] != "poison-message" ||
				call.publishing.Headers[rabbitDLQHeaderOriginalAttempt] != rabbitTestAttemptA ||
				call.publishing.Headers[rabbitDLQHeaderQueueTenantHash] != topology.tenantHash {
				t.Fatalf("DLQ safe metadata headers = %+v", call.publishing.Headers)
			}
			var envelope deadLetterEnvelopeV1
			if err := json.Unmarshal(call.publishing.Body, &envelope); err != nil {
				t.Fatalf("decode DLQ envelope: %v", err)
			}
			if envelope.RegisteredResource != message.Resource || envelope.ReasonCode != "poison-message" || envelope.RawBodyOmitted != test.omitted {
				t.Fatalf("DLQ envelope = %+v", envelope)
			}
			if test.omitted {
				digest := sha256.Sum256(test.body)
				if len(envelope.RawBody) != 0 || envelope.RawBodySize != int64(len(test.body)) || envelope.RawBodySHA256 != hex.EncodeToString(digest[:]) {
					t.Fatalf("omitted DLQ evidence = %+v", envelope)
				}
			} else if !bytes.Equal(envelope.RawBody, test.body) {
				t.Fatalf("retained DLQ body = %q, want %q", envelope.RawBody, test.body)
			}
		})
	}
}

func TestRabbitReadyDepthCloseAndRegistryValidation(t *testing.T) {
	channel := newRabbitTestChannel()
	channel.inspect = amqp.Queue{Messages: 7}
	connection := &rabbitTestConnection{channel: channel}
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
	client := newRabbitTestClient(t, dialer, rabbitTestAttemptA)

	invalid := rabbitTestMessage()
	invalid.TaskID = "not-a-rag-id"
	invalid.DispatchToken.TaskID = invalid.TaskID
	if _, err := client.PublishMandatoryConfirmed(context.Background(), invalid); err == nil {
		t.Fatal("resource-specific invalid task ID was accepted")
	}
	if dialer.callCount() != 0 {
		t.Fatal("invalid registry input performed RabbitMQ I/O")
	}
	if _, _, err := client.GetOne(context.Background(), "unknown.resource", "tenant-a"); err == nil {
		t.Fatal("unknown resource was accepted")
	}
	if dialer.callCount() != 0 {
		t.Fatal("unknown resource performed RabbitMQ I/O")
	}

	depth, err := client.ReadyDepth(context.Background(), "rag.index", "tenant-a")
	if err != nil || depth != 7 {
		t.Fatalf("ReadyDepth() = %d, %v, want 7", depth, err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if connection.closeCalls.Load() != 1 {
		t.Fatalf("connection Close calls = %d, want 1", connection.closeCalls.Load())
	}
	if _, err := client.ReadyDepth(context.Background(), "rag.index", "tenant-a"); !errors.Is(err, ErrDependencyUnavailable) {
		t.Fatalf("ReadyDepth after Close error = %v", err)
	}
}

func TestRabbitCloseDeadlineErrorForcesAbort(t *testing.T) {
	channel := newRabbitTestChannel()
	connection := &rabbitTestConnection{
		channel:  channel,
		closeErr: errors.New("graceful close failed"),
	}
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
	client := newRabbitTestClient(t, dialer, rabbitTestAttemptA)

	if err := client.EnsureTenantTopology(context.Background(), "rag.index", "tenant-a"); err != nil {
		t.Fatalf("EnsureTenantTopology() error = %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if connection.closeCalls.Load() != 1 || connection.abortCalls.Load() != 1 {
		t.Fatalf(
			"close/abort calls = %d/%d, want 1/1",
			connection.closeCalls.Load(), connection.abortCalls.Load(),
		)
	}
}

func TestRabbitOperationGateDeadlineDoesNotTouchBroker(t *testing.T) {
	dialer := &rabbitTestDialer{}
	client := newRabbitTestClientWithOptions(t, dialer, RabbitOptions{
		URL: "amqp://unit.test/", OperationTimeout: 30 * time.Millisecond,
	}, rabbitTestAttemptA)
	if err := client.operationGate.acquire(context.Background()); err != nil {
		t.Fatalf("hold operation gate: %v", err)
	}
	gateReleased := false
	defer func() {
		if !gateReleased {
			client.operationGate.release()
		}
	}()

	receipt, err := client.PublishMandatoryConfirmed(context.Background(), rabbitTestMessage())
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrPublishUnconfirmed) || receipt.AttemptID != rabbitTestAttemptA {
		t.Fatalf("publish waiting for operation gate = receipt:%+v err:%v", receipt, err)
	}
	if dialer.callCount() != 0 {
		t.Fatalf("operation waiting for gate performed %d broker dials", dialer.callCount())
	}
}

func TestRabbitReadOperationGateDeadlinesAreTypedAndDoNotTouchBroker(t *testing.T) {
	dialer := &rabbitTestDialer{}
	client := newRabbitTestClient(t, dialer)
	if err := client.operationGate.acquire(context.Background()); err != nil {
		t.Fatalf("hold operation gate: %v", err)
	}
	defer client.operationGate.release()

	tests := []struct {
		name string
		call func(context.Context) error
	}{
		{name: "ensure topology", call: func(ctx context.Context) error {
			return client.EnsureTenantTopology(ctx, "rag.index", "tenant-a")
		}},
		{name: "get one", call: func(ctx context.Context) error {
			_, _, err := client.GetOne(ctx, "rag.index", "tenant-a")
			return err
		}},
		{name: "ready depth", call: func(ctx context.Context) error {
			_, err := client.ReadyDepth(ctx, "rag.index", "tenant-a")
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := test.call(ctx)
			if !errors.Is(err, ErrDependencyUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("operation gate deadline error = %v", err)
			}
		})
	}
	if dialer.callCount() != 0 {
		t.Fatalf("read operations waiting for gate performed %d broker dials", dialer.callCount())
	}
}

func TestRabbitBlockedConnectionAndTopologyRPCsAbortAtDeadline(t *testing.T) {
	t.Run("open channel", func(t *testing.T) {
		channel := newRabbitTestChannel()
		openGate := make(chan struct{})
		connection := &rabbitTestConnection{channel: channel, openGate: openGate, aborted: make(chan struct{})}
		dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
		client := newRabbitTestClientWithOptions(t, dialer, RabbitOptions{
			URL: "amqp://unit.test/", OperationTimeout: 30 * time.Millisecond,
		})

		err := client.EnsureTenantTopology(context.Background(), "rag.index", "tenant-a")
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrDependencyUnavailable) {
			t.Fatalf("blocked OpenChannel error = %v", err)
		}
		if connection.abortCalls.Load() != 1 {
			t.Fatalf("blocked OpenChannel Abort calls = %d, want 1", connection.abortCalls.Load())
		}
		if len(channel.snapshotOps()) != 0 {
			t.Fatalf("blocked OpenChannel reached channel RPCs: %+v", channel.snapshotOps())
		}
	})

	t.Run("exchange declare", func(t *testing.T) {
		channel := newRabbitTestChannel()
		channel.rpcGates[rabbitTestExchangeDeclare] = make(chan struct{})
		connection := &rabbitTestConnection{channel: channel, aborted: make(chan struct{})}
		dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
		client := newRabbitTestClientWithOptions(t, dialer, RabbitOptions{
			URL: "amqp://unit.test/", OperationTimeout: 30 * time.Millisecond,
		})

		err := client.EnsureTenantTopology(context.Background(), "rag.index", "tenant-a")
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrDependencyUnavailable) {
			t.Fatalf("blocked ExchangeDeclare error = %v", err)
		}
		if connection.abortCalls.Load() != 1 {
			t.Fatalf("blocked ExchangeDeclare Abort calls = %d, want 1", connection.abortCalls.Load())
		}
		operations := channel.snapshotOps()
		if len(operations) != 2 || operations[0].kind != rabbitTestConfirm || operations[1].kind != rabbitTestExchangeDeclare {
			t.Fatalf("blocked declaration operations = %+v", operations)
		}
	})
}

func TestRabbitPreconditionFailedMapsUnsupportedTopology(t *testing.T) {
	precondition := &amqp.Error{Code: amqp.PreconditionFailed, Reason: "inequivalent durable topology"}

	t.Run("topology operation", func(t *testing.T) {
		channel := newRabbitTestChannel()
		channel.rpcErrors[rabbitTestExchangeDeclare] = precondition
		connection := &rabbitTestConnection{channel: channel, aborted: make(chan struct{})}
		dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
		client := newRabbitTestClient(t, dialer)

		err := client.EnsureTenantTopology(context.Background(), "rag.index", "tenant-a")
		if !errors.Is(err, ErrUnsupportedTopology) {
			t.Fatalf("AMQP 406 topology error = %v", err)
		}
		if connection.abortCalls.Load() != 1 {
			t.Fatalf("AMQP 406 Abort calls = %d, want 1", connection.abortCalls.Load())
		}
	})

	t.Run("publish retains unconfirmed category and receipt", func(t *testing.T) {
		channel := newRabbitTestChannel()
		channel.rpcErrors[rabbitTestExchangeDeclare] = precondition
		connection := &rabbitTestConnection{channel: channel, aborted: make(chan struct{})}
		dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
		client := newRabbitTestClient(t, dialer, rabbitTestAttemptA)

		receipt, err := client.PublishMandatoryConfirmed(context.Background(), rabbitTestMessage())
		if !errors.Is(err, ErrUnsupportedTopology) || !errors.Is(err, ErrPublishUnconfirmed) || receipt.AttemptID != rabbitTestAttemptA {
			t.Fatalf("AMQP 406 publish = receipt:%+v err:%v", receipt, err)
		}
	})
}

func TestRabbitIdleChannelCloseForcesFreshGenerationAndTopology(t *testing.T) {
	firstChannel := newRabbitTestChannel()
	secondChannel := newRabbitTestChannel()
	firstConnection := &rabbitTestConnection{channel: firstChannel, aborted: make(chan struct{})}
	secondConnection := &rabbitTestConnection{channel: secondChannel, aborted: make(chan struct{})}
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: firstConnection}, {connection: secondConnection}}}
	client := newRabbitTestClient(t, dialer)

	if err := client.EnsureTenantTopology(context.Background(), "rag.index", "tenant-a"); err != nil {
		t.Fatalf("initial EnsureTenantTopology() error = %v", err)
	}
	client.stateMu.Lock()
	firstGeneration := client.generation
	client.stateMu.Unlock()
	firstChannel.emitClose(t)
	select {
	case <-firstGeneration.done:
	case <-time.After(time.Second):
		t.Fatal("idle close watcher did not mark generation dead")
	}
	select {
	case <-firstConnection.aborted:
	case <-time.After(time.Second):
		t.Fatal("idle close watcher did not abort the old connection")
	}
	if firstConnection.abortCalls.Load() != 1 {
		t.Fatalf("idle close Abort calls = %d, want 1", firstConnection.abortCalls.Load())
	}

	if err := client.EnsureTenantTopology(context.Background(), "rag.index", "tenant-a"); err != nil {
		t.Fatalf("EnsureTenantTopology after idle close error = %v", err)
	}
	if dialer.callCount() != 2 {
		t.Fatalf("dial count after idle close = %d, want 2", dialer.callCount())
	}
	if rabbitTestDeclarationCount(secondChannel.snapshotOps()) == 0 {
		t.Fatal("first operation after idle close reused stale topology cache")
	}
}

func TestRabbitSettlementFailureAbortsOwningGeneration(t *testing.T) {
	tests := []struct {
		name       string
		settle     func(Delivery, context.Context) error
		ackError   error
		blocked    bool
		wantCtxErr error
	}{
		{
			name: "ack error", settle: func(delivery Delivery, ctx context.Context) error { return delivery.Ack(ctx) },
			ackError: errors.New("ack failed"),
		},
		{
			name:    "nack blocked until context cancellation",
			settle:  func(delivery Delivery, ctx context.Context) error { return delivery.Nack(ctx, true) },
			blocked: true, wantCtxErr: context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := newRabbitTestChannel()
			connection := &rabbitTestConnection{channel: channel, aborted: make(chan struct{})}
			dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
			client := newRabbitTestClient(t, dialer)
			acknowledger := &rabbitTestAcknowledger{err: test.ackError}
			var release chan struct{}
			if test.blocked {
				release = make(chan struct{})
				acknowledger.gate = release
				acknowledger.started = make(chan struct{}, 1)
			}
			channel.enqueueGet(rabbitTestGetResult{
				delivery: rabbitTestDelivery(t, rabbitTestMessage(), rabbitTestAttemptA, acknowledger), ok: true,
			})
			delivery, ok, err := client.GetOne(context.Background(), "rag.index", "tenant-a")
			if err != nil || !ok {
				t.Fatalf("GetOne() = %v, %v, %v", delivery, ok, err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			result := make(chan error, 1)
			go func() { result <- test.settle(delivery, ctx) }()
			if test.blocked {
				select {
				case <-acknowledger.started:
				case <-time.After(time.Second):
					t.Fatal("settlement did not reach acknowledger")
				}
				cancel()
			} else {
				defer cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, ErrDependencyUnavailable) {
					t.Fatalf("settlement error = %v", err)
				}
				if test.wantCtxErr != nil && !errors.Is(err, test.wantCtxErr) {
					t.Fatalf("settlement error = %v, want %v", err, test.wantCtxErr)
				}
			case <-time.After(time.Second):
				t.Fatal("settlement did not return")
			}
			if release != nil {
				close(release)
			}
			if connection.abortCalls.Load() != 1 {
				t.Fatalf("settlement Abort calls = %d, want 1", connection.abortCalls.Load())
			}
			client.stateMu.Lock()
			generation := client.generation
			client.stateMu.Unlock()
			if generation != nil {
				t.Fatal("settlement failure left old generation active")
			}
		})
	}
}

func TestRabbitSettlementGateDeadlineAbortsOwningGeneration(t *testing.T) {
	channel := newRabbitTestChannel()
	connection := &rabbitTestConnection{channel: channel, aborted: make(chan struct{})}
	dialer := &rabbitTestDialer{results: []rabbitTestDialResult{{connection: connection}}}
	client := newRabbitTestClient(t, dialer)
	channel.enqueueGet(rabbitTestGetResult{
		delivery: rabbitTestDelivery(t, rabbitTestMessage(), rabbitTestAttemptA, &rabbitTestAcknowledger{}),
		ok:       true,
	})
	delivery, ok, err := client.GetOne(context.Background(), "rag.index", "tenant-a")
	if err != nil || !ok {
		t.Fatalf("GetOne() = %v, %v, %v", delivery, ok, err)
	}
	if err := client.operationGate.acquire(context.Background()); err != nil {
		t.Fatalf("hold operation gate: %v", err)
	}
	gateReleased := false
	defer func() {
		if !gateReleased {
			client.operationGate.release()
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = delivery.Ack(ctx)
	if !errors.Is(err, ErrDependencyUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ack waiting for operation gate error = %v", err)
	}
	if connection.abortCalls.Load() != 1 {
		t.Fatalf("settlement gate deadline Abort calls = %d, want 1", connection.abortCalls.Load())
	}
	client.operationGate.release()
	gateReleased = true
	if err := delivery.Ack(context.Background()); !errors.Is(err, ErrDeliverySettled) {
		t.Fatalf("second Ack error = %v, want ErrDeliverySettled", err)
	}
}

var rabbitIntegrationSequence atomic.Uint64

type rabbitIntegrationFixture struct {
	url                string
	resource           string
	exchange           string
	deadLetterExchange string
	registry           *Registry
	tenants            map[string]struct{}
	clients            []*Rabbit
}

func newRabbitIntegrationFixture(t *testing.T, url string) *rabbitIntegrationFixture {
	t.Helper()
	suffix := fmt.Sprintf("%x.%x", uint64(time.Now().UnixNano()), rabbitIntegrationSequence.Add(1))
	resource := "testrabbit." + suffix
	config := rabbitTestConfig()
	config.Key = resource
	config.ValidateTaskID = func(taskID string) bool { return taskID == "task-1" || taskID == "task-2" }
	registry, err := NewRegistry(config)
	if err != nil {
		t.Fatalf("create integration registry: %v", err)
	}
	fixture := &rabbitIntegrationFixture{
		url: url, resource: resource,
		exchange: "bkcrab.test.fair.task." + suffix, deadLetterExchange: "bkcrab.test.fair.dlx." + suffix,
		registry: registry, tenants: make(map[string]struct{}),
	}
	t.Cleanup(func() { fixture.cleanup(t) })
	return fixture
}

func (f *rabbitIntegrationFixture) newClient(t *testing.T) *Rabbit {
	t.Helper()
	client, err := NewRabbit(RabbitOptions{
		URL: f.url, Exchange: f.exchange, DeadLetterExchange: f.deadLetterExchange,
		OperationTimeout: 10 * time.Second,
	}, f.registry)
	if err != nil {
		t.Fatalf("create Rabbit integration client: %v", err)
	}
	f.clients = append(f.clients, client)
	return client
}

func (f *rabbitIntegrationFixture) message(tenant string, generation uint64) Message {
	f.tenants[tenant] = struct{}{}
	return Message{
		Version: MessageVersion1, Resource: f.resource, TenantID: tenant,
		TaskType: "test.task", TaskID: "task-1",
		DispatchToken: DispatchToken{Resource: f.resource, TaskID: "task-1", Generation: generation},
	}
}

func (f *rabbitIntegrationFixture) unbindTenant(t *testing.T, tenant string) {
	t.Helper()
	connection, err := amqp.Dial(f.url)
	if err != nil {
		t.Fatalf("dial Rabbit integration control connection: %v", err)
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		t.Fatalf("open Rabbit integration control channel: %v", err)
	}
	defer channel.Close()
	queue, err := TenantQueueName(f.resource, tenant)
	if err != nil {
		t.Fatalf("build integration tenant queue: %v", err)
	}
	routingKey, err := TenantRoutingKey(f.resource, tenant)
	if err != nil {
		t.Fatalf("build integration routing key: %v", err)
	}
	if err := channel.QueueUnbind(queue, routingKey, f.exchange, nil); err != nil {
		t.Fatalf("remove integration tenant binding: %v", err)
	}
}

func (f *rabbitIntegrationFixture) canonicalPublishing(t *testing.T, message Message, attemptID string) amqp.Publishing {
	t.Helper()
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal integration message: %v", err)
	}
	return amqp.Publishing{
		Headers: amqp.Table{
			rabbitHeaderProtocolVersion: int32(MessageVersion1),
			rabbitHeaderResource:        message.Resource,
			rabbitHeaderTaskID:          message.TaskID,
			rabbitHeaderDispatchGeneration: int64(
				message.DispatchToken.Generation,
			),
		},
		ContentType: "application/json", DeliveryMode: amqp.Persistent,
		MessageId: attemptID, CorrelationId: attemptID,
		Type: "bkcrab.fair.v1", AppId: "bkcrab", Body: body,
	}
}

func (f *rabbitIntegrationFixture) publishRawConfirmed(
	t *testing.T,
	client *Rabbit,
	tenant string,
	publishing amqp.Publishing,
) {
	t.Helper()
	f.tenants[tenant] = struct{}{}
	if err := client.EnsureTenantTopology(context.Background(), f.resource, tenant); err != nil {
		t.Fatalf("ensure raw-publish topology: %v", err)
	}
	connection, err := amqp.Dial(f.url)
	if err != nil {
		t.Fatalf("dial Rabbit raw-publish connection: %v", err)
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		t.Fatalf("open Rabbit raw-publish channel: %v", err)
	}
	defer channel.Close()
	if err := channel.Confirm(false); err != nil {
		t.Fatalf("enable raw-publish confirms: %v", err)
	}
	confirms := channel.NotifyPublish(make(chan amqp.Confirmation, 1))
	returns := channel.NotifyReturn(make(chan amqp.Return, 1))
	routingKey, err := TenantRoutingKey(f.resource, tenant)
	if err != nil {
		t.Fatalf("build raw-publish routing key: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := channel.PublishWithContext(ctx, f.exchange, routingKey, true, false, publishing); err != nil {
		t.Fatalf("raw mandatory publish: %v", err)
	}
	returned := false
	for {
		select {
		case returnedMessage := <-returns:
			returned = true
			t.Logf("unexpected raw mandatory return code=%d key=%q", returnedMessage.ReplyCode, returnedMessage.RoutingKey)
		case confirmation := <-confirms:
			select {
			case <-returns:
				returned = true
			default:
			}
			if !confirmation.Ack || returned {
				t.Fatalf("raw publish outcome ack=%t returned=%t", confirmation.Ack, returned)
			}
			return
		case <-ctx.Done():
			t.Fatalf("raw publish confirmation deadline: %v", ctx.Err())
		}
	}
}

func (f *rabbitIntegrationFixture) inspectAndAckQueue(
	t *testing.T,
	queue string,
	inspect func(amqp.Delivery),
) {
	t.Helper()
	connection, err := amqp.Dial(f.url)
	if err != nil {
		t.Fatalf("dial Rabbit queue-inspection connection: %v", err)
	}
	defer connection.Close()
	channel, err := connection.Channel()
	if err != nil {
		t.Fatalf("open Rabbit queue-inspection channel: %v", err)
	}
	defer channel.Close()
	delivery, ok, err := channel.Get(queue, false)
	if err != nil || !ok {
		t.Fatalf("raw Get(%q) = %t, %v", queue, ok, err)
	}
	inspect(delivery)
	if err := delivery.Ack(false); err != nil {
		t.Fatalf("ack inspected queue delivery: %v", err)
	}
}

func (f *rabbitIntegrationFixture) cleanup(t *testing.T) {
	t.Helper()
	for _, client := range f.clients {
		_ = client.Close()
	}
	connection, err := amqp.Dial(f.url)
	if err != nil {
		t.Logf("Rabbit integration cleanup could not dial broker: %v", err)
		return
	}
	defer connection.Close()
	for tenant := range f.tenants {
		queue, buildErr := TenantQueueName(f.resource, tenant)
		if buildErr == nil {
			f.cleanupControl(t, connection, "tenant queue "+queue, func(channel *amqp.Channel) error {
				_, deleteErr := channel.QueueDelete(queue, false, false, false)
				return deleteErr
			})
		}
	}
	if queue, buildErr := DeadLetterQueueName(f.resource); buildErr == nil {
		f.cleanupControl(t, connection, "DLQ "+queue, func(channel *amqp.Channel) error {
			_, deleteErr := channel.QueueDelete(queue, false, false, false)
			return deleteErr
		})
	}
	f.cleanupControl(t, connection, "task exchange "+f.exchange, func(channel *amqp.Channel) error {
		return channel.ExchangeDelete(f.exchange, false, false)
	})
	f.cleanupControl(t, connection, "DLX "+f.deadLetterExchange, func(channel *amqp.Channel) error {
		return channel.ExchangeDelete(f.deadLetterExchange, false, false)
	})
}

func (f *rabbitIntegrationFixture) cleanupControl(
	t *testing.T,
	connection *amqp.Connection,
	object string,
	remove func(*amqp.Channel) error,
) {
	t.Helper()
	channel, err := connection.Channel()
	if err != nil {
		t.Logf("Rabbit integration cleanup could not open channel for owned %s: %v", object, err)
		return
	}
	defer channel.Close()
	if err := remove(channel); err != nil {
		t.Logf("Rabbit integration cleanup could not delete owned %s: %v", object, err)
	}
}

func TestRabbitIntegration(t *testing.T) {
	url := os.Getenv("BKCRAB_TEST_RABBITMQ_URL")
	if url == "" {
		t.Skip("BKCRAB_TEST_RABBITMQ_URL is not set")
	}

	t.Run("publish confirm get ack", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		client := fixture.newClient(t)
		message := fixture.message("tenant-a", 1)
		receipt, err := client.PublishMandatoryConfirmed(context.Background(), message)
		if err != nil || receipt.Validate() != nil {
			t.Fatalf("PublishMandatoryConfirmed() = %+v, %v", receipt, err)
		}
		delivery, ok, err := client.GetOne(context.Background(), fixture.resource, message.TenantID)
		if err != nil || !ok {
			t.Fatalf("GetOne() = %v, %t, %v", delivery, ok, err)
		}
		request := delivery.Request()
		if request.Message == nil || *request.Message != message {
			t.Fatalf("delivered executable message = %+v", request.Message)
		}
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("Ack() error = %v", err)
		}
	})

	t.Run("tenant queues are isolated", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		client := fixture.newClient(t)
		messageA := fixture.message("tenant-a", 1)
		messageB := fixture.message("tenant-b", 2)
		if _, err := client.PublishMandatoryConfirmed(context.Background(), messageA); err != nil {
			t.Fatalf("publish tenant A: %v", err)
		}
		if _, err := client.PublishMandatoryConfirmed(context.Background(), messageB); err != nil {
			t.Fatalf("publish tenant B: %v", err)
		}
		if depth, err := client.ReadyDepth(context.Background(), fixture.resource, messageA.TenantID); err != nil || depth != 1 {
			t.Fatalf("tenant A depth = %d, %v", depth, err)
		}
		if depth, err := client.ReadyDepth(context.Background(), fixture.resource, messageB.TenantID); err != nil || depth != 1 {
			t.Fatalf("tenant B depth = %d, %v", depth, err)
		}
		for _, message := range []Message{messageA, messageB} {
			delivery, ok, err := client.GetOne(context.Background(), fixture.resource, message.TenantID)
			if err != nil || !ok || delivery.Request().Message == nil || delivery.Request().Message.TenantID != message.TenantID {
				t.Fatalf("isolated GetOne(%q) = %v, %t, %v", message.TenantID, delivery, ok, err)
			}
			if err := delivery.Ack(context.Background()); err != nil {
				t.Fatalf("Ack(%q) error = %v", message.TenantID, err)
			}
		}
	})

	t.Run("corrupt body retains raw and stable header locator", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		client := fixture.newClient(t)
		message := fixture.message("tenant-corrupt-body", 1)
		publishing := fixture.canonicalPublishing(t, message, rabbitTestAttemptA)
		publishing.Body = []byte("{corrupt-json")
		fixture.publishRawConfirmed(t, client, message.TenantID, publishing)

		delivery, ok, err := client.GetOne(context.Background(), fixture.resource, message.TenantID)
		if err != nil || !ok {
			t.Fatalf("GetOne corrupt body = %v, %t, %v", delivery, ok, err)
		}
		request := delivery.Request()
		if request.Message != nil || request.BodyCandidate != nil || request.HeaderToken == nil ||
			*request.HeaderToken != message.DispatchToken || !bytes.Equal(request.RawBody, publishing.Body) ||
			request.DecodeErrorCode == "" {
			t.Fatalf("corrupt-body transport facts = %+v", request)
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("corrupt-body request validation: %v", err)
		}
		// Runtime repair/DLQ policy is implemented by later tasks. This direct ACK
		// only settles the integration fixture after transport facts are asserted.
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("settle corrupt-body fixture: %v", err)
		}
	})

	t.Run("valid body with invalid header retains only body locator", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		client := fixture.newClient(t)
		message := fixture.message("tenant-invalid-header", 1)
		publishing := fixture.canonicalPublishing(t, message, rabbitTestAttemptA)
		publishing.Headers[rabbitHeaderTaskID] = []byte(message.TaskID)
		fixture.publishRawConfirmed(t, client, message.TenantID, publishing)

		delivery, ok, err := client.GetOne(context.Background(), fixture.resource, message.TenantID)
		if err != nil || !ok {
			t.Fatalf("GetOne invalid header = %v, %t, %v", delivery, ok, err)
		}
		request := delivery.Request()
		if request.Message != nil || request.BodyCandidate == nil || *request.BodyCandidate != message ||
			request.HeaderToken != nil || request.HeaderErrorCode == "" {
			t.Fatalf("invalid-header transport facts = %+v", request)
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("invalid-header request validation: %v", err)
		}
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("settle invalid-header fixture: %v", err)
		}
	})

	t.Run("invalid attempt properties retain body and header locators", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		client := fixture.newClient(t)
		message := fixture.message("tenant-invalid-properties", 1)
		publishing := fixture.canonicalPublishing(t, message, rabbitTestAttemptA)
		publishing.CorrelationId = rabbitTestAttemptB
		fixture.publishRawConfirmed(t, client, message.TenantID, publishing)

		delivery, ok, err := client.GetOne(context.Background(), fixture.resource, message.TenantID)
		if err != nil || !ok {
			t.Fatalf("GetOne invalid properties = %v, %t, %v", delivery, ok, err)
		}
		request := delivery.Request()
		if request.Message != nil || request.BodyCandidate == nil || *request.BodyCandidate != message ||
			request.HeaderToken == nil || *request.HeaderToken != message.DispatchToken ||
			request.PublishAttemptID != "" || request.PropertyErrorCode == "" {
			t.Fatalf("invalid-property transport facts = %+v", request)
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("invalid-property request validation: %v", err)
		}
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("settle invalid-property fixture: %v", err)
		}
	})

	t.Run("valid mismatched body and header retain both locators", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		client := fixture.newClient(t)
		message := fixture.message("tenant-mismatch", 1)
		publishing := fixture.canonicalPublishing(t, message, rabbitTestAttemptA)
		publishing.Headers[rabbitHeaderTaskID] = "task-2"
		fixture.publishRawConfirmed(t, client, message.TenantID, publishing)

		delivery, ok, err := client.GetOne(context.Background(), fixture.resource, message.TenantID)
		if err != nil || !ok {
			t.Fatalf("GetOne mismatched locators = %v, %t, %v", delivery, ok, err)
		}
		request := delivery.Request()
		wantHeader := DispatchToken{Resource: fixture.resource, TaskID: "task-2", Generation: 1}
		if request.Message != nil || request.BodyCandidate == nil || *request.BodyCandidate != message ||
			request.HeaderToken == nil || *request.HeaderToken != wantHeader || request.HeaderErrorCode != "" {
			t.Fatalf("mismatched transport facts = %+v", request)
		}
		if err := request.Validate(); err != nil {
			t.Fatalf("mismatched request validation: %v", err)
		}
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("settle mismatched fixture: %v", err)
		}
	})

	t.Run("poison confirmed DLQ then original ACK", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		client := fixture.newClient(t)
		message := fixture.message("tenant-poison", 1)
		publishing := fixture.canonicalPublishing(t, message, rabbitTestAttemptA)
		publishing.Body = []byte("{poison")
		fixture.publishRawConfirmed(t, client, message.TenantID, publishing)

		delivery, ok, err := client.GetOne(context.Background(), fixture.resource, message.TenantID)
		if err != nil || !ok {
			t.Fatalf("GetOne poison = %v, %t, %v", delivery, ok, err)
		}
		request := DeadLetterRequest{Delivery: delivery.Request(), ReasonCode: "integration-poison"}
		receipt, err := client.PublishDeadLetterConfirmed(context.Background(), request)
		if err != nil || receipt.Validate() != nil {
			t.Fatalf("PublishDeadLetterConfirmed() = %+v, %v", receipt, err)
		}
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("ACK original poison after confirmed DLQ: %v", err)
		}
		dlq, err := DeadLetterQueueName(fixture.resource)
		if err != nil {
			t.Fatalf("build integration DLQ name: %v", err)
		}
		fixture.inspectAndAckQueue(t, dlq, func(deadLetter amqp.Delivery) {
			if deadLetter.DeliveryMode != amqp.Persistent || deadLetter.Headers[rabbitDLQHeaderReason] != "integration-poison" {
				t.Fatalf("DLQ AMQP envelope properties = %+v", deadLetter)
			}
			var envelope deadLetterEnvelopeV1
			if err := json.Unmarshal(deadLetter.Body, &envelope); err != nil {
				t.Fatalf("decode retained poison DLQ envelope: %v", err)
			}
			if envelope.RawBodyOmitted || !bytes.Equal(envelope.RawBody, publishing.Body) ||
				envelope.HeaderToken == nil || *envelope.HeaderToken != message.DispatchToken {
				t.Fatalf("retained poison DLQ envelope = %+v", envelope)
			}
		})
	})

	t.Run("oversized poison confirmed DLQ then original ACK", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		client := fixture.newClient(t)
		message := fixture.message("tenant-oversized-poison", 1)
		publishing := fixture.canonicalPublishing(t, message, rabbitTestAttemptA)
		publishing.Body = bytes.Repeat([]byte{'z'}, MaxRawDeliveryBytes+1)
		wantDigest := sha256.Sum256(publishing.Body)
		fixture.publishRawConfirmed(t, client, message.TenantID, publishing)

		delivery, ok, err := client.GetOne(context.Background(), fixture.resource, message.TenantID)
		if err != nil || !ok {
			t.Fatalf("GetOne oversized poison = %v, %t, %v", delivery, ok, err)
		}
		facts := delivery.Request()
		if !facts.RawBodyOmitted || len(facts.RawBody) != 0 || facts.RawBodySize != int64(len(publishing.Body)) ||
			facts.RawBodySHA256 != hex.EncodeToString(wantDigest[:]) || facts.BodyCandidate != nil {
			t.Fatalf("oversized transport facts = %+v", facts)
		}
		receipt, err := client.PublishDeadLetterConfirmed(context.Background(), DeadLetterRequest{
			Delivery: facts, ReasonCode: "integration-oversized-poison",
		})
		if err != nil || receipt.Validate() != nil {
			t.Fatalf("PublishDeadLetterConfirmed oversized = %+v, %v", receipt, err)
		}
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("ACK original oversized poison after confirmed DLQ: %v", err)
		}
		dlq, err := DeadLetterQueueName(fixture.resource)
		if err != nil {
			t.Fatalf("build oversized integration DLQ name: %v", err)
		}
		fixture.inspectAndAckQueue(t, dlq, func(deadLetter amqp.Delivery) {
			var envelope deadLetterEnvelopeV1
			if err := json.Unmarshal(deadLetter.Body, &envelope); err != nil {
				t.Fatalf("decode oversized poison DLQ envelope: %v", err)
			}
			if !envelope.RawBodyOmitted || len(envelope.RawBody) != 0 ||
				envelope.RawBodySize != int64(len(publishing.Body)) || envelope.RawBodySHA256 != hex.EncodeToString(wantDigest[:]) {
				t.Fatalf("oversized poison DLQ envelope = %+v", envelope)
			}
		})
	})

	t.Run("connection rebuild retains durable message", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		publisher := fixture.newClient(t)
		message := fixture.message("tenant-rebuild", 1)
		if _, err := publisher.PublishMandatoryConfirmed(context.Background(), message); err != nil {
			t.Fatalf("publish before connection rebuild: %v", err)
		}
		if err := publisher.Close(); err != nil {
			t.Fatalf("close publisher connection: %v", err)
		}
		consumer := fixture.newClient(t)
		delivery, ok, err := consumer.GetOne(context.Background(), fixture.resource, message.TenantID)
		if err != nil || !ok || delivery.Request().Message == nil || *delivery.Request().Message != message {
			t.Fatalf("GetOne after connection rebuild = %v, %t, %v", delivery, ok, err)
		}
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("Ack after connection rebuild: %v", err)
		}
	})

	t.Run("deleted binding returns mandatory publish and next attempt repairs", func(t *testing.T) {
		fixture := newRabbitIntegrationFixture(t, url)
		client := fixture.newClient(t)
		message := fixture.message("tenant-return", 1)
		if err := client.EnsureTenantTopology(context.Background(), fixture.resource, message.TenantID); err != nil {
			t.Fatalf("EnsureTenantTopology() error = %v", err)
		}
		fixture.unbindTenant(t, message.TenantID)
		receipt, err := client.PublishMandatoryConfirmed(context.Background(), message)
		if receipt.Validate() != nil || !errors.Is(err, ErrPublishUnroutable) {
			t.Fatalf("publish after binding deletion = %+v, %v", receipt, err)
		}
		message.DispatchToken.Generation++
		if _, err := client.PublishMandatoryConfirmed(context.Background(), message); err != nil {
			t.Fatalf("publish after topology cache repair: %v", err)
		}
		delivery, ok, err := client.GetOne(context.Background(), fixture.resource, message.TenantID)
		if err != nil || !ok {
			t.Fatalf("GetOne after topology cache repair = %v, %t, %v", delivery, ok, err)
		}
		if err := delivery.Ack(context.Background()); err != nil {
			t.Fatalf("Ack repaired publish: %v", err)
		}
	})
}

func assertRabbitTestTopology(t *testing.T, operations []rabbitTestOp, message Message) {
	t.Helper()
	wantQueue, _ := TenantQueueName(message.Resource, message.TenantID)
	wantRouting, _ := TenantRoutingKey(message.Resource, message.TenantID)
	wantDLQ, _ := DeadLetterQueueName(message.Resource)
	foundTaskExchange := false
	foundDLX := false
	foundTenantQueue := false
	foundTenantBinding := false
	foundDLQ := false
	foundDLQBinding := false
	for _, operation := range operations {
		switch operation.kind {
		case rabbitTestExchangeDeclare:
			if operation.name == "test.fair.task" && operation.routingKey == "direct" && operation.durable && !operation.autoDelete {
				foundTaskExchange = true
			}
			if operation.name == "test.fair.dlx" && operation.routingKey == "direct" && operation.durable && !operation.autoDelete {
				foundDLX = true
			}
		case rabbitTestQueueDeclare:
			if operation.name == wantQueue && operation.durable && !operation.autoDelete && !operation.exclusive &&
				operation.args["x-dead-letter-exchange"] == "test.fair.dlx" && operation.args["x-dead-letter-routing-key"] == message.Resource {
				foundTenantQueue = true
			}
			if operation.name == wantDLQ && operation.durable && !operation.autoDelete && !operation.exclusive {
				foundDLQ = true
			}
		case rabbitTestQueueBind:
			if operation.name == wantQueue && operation.exchange == "test.fair.task" && operation.routingKey == wantRouting {
				foundTenantBinding = true
			}
			if operation.name == wantDLQ && operation.exchange == "test.fair.dlx" && operation.routingKey == message.Resource {
				foundDLQBinding = true
			}
		}
	}
	if !foundTaskExchange || !foundDLX || !foundTenantQueue || !foundTenantBinding || !foundDLQ || !foundDLQBinding {
		t.Fatalf("incomplete durable topology: taskExchange=%t dlx=%t tenantQueue=%t tenantBind=%t dlq=%t dlqBind=%t\nops=%+v",
			foundTaskExchange, foundDLX, foundTenantQueue, foundTenantBinding, foundDLQ, foundDLQBinding, operations)
	}
}

func rabbitTestDeclarationCount(operations []rabbitTestOp) int {
	count := 0
	for _, operation := range operations {
		if operation.kind == rabbitTestExchangeDeclare || operation.kind == rabbitTestQueueDeclare || operation.kind == rabbitTestQueueBind {
			count++
		}
	}
	return count
}

func (operation rabbitTestOp) String() string {
	return fmt.Sprintf("%s(%s,%s,%s)", operation.kind, operation.name, operation.exchange, operation.routingKey)
}

var _ rabbitDialer = (*rabbitTestDialer)(nil)
var _ rabbitConnection = (*rabbitTestConnection)(nil)
var _ rabbitChannel = (*rabbitTestChannel)(nil)
var _ rabbitAttemptIDSource = (*rabbitTestAttemptIDs)(nil)
var _ amqp.Acknowledger = (*rabbitTestAcknowledger)(nil)
