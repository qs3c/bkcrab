package fairqueue

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	DefaultRabbitExchange           = "bkcrab.fair.task"
	DefaultRabbitDeadLetterExchange = "bkcrab.fair.dlx"

	rabbitHeaderProtocolVersion    = "x-bkcrab-protocol-version"
	rabbitHeaderResource           = "x-bkcrab-resource"
	rabbitHeaderTaskID             = "x-bkcrab-task-id"
	rabbitHeaderDispatchGeneration = "x-bkcrab-dispatch-generation"

	rabbitDLQHeaderReason            = "x-bkcrab-dlq-reason"
	rabbitDLQHeaderOriginalAttempt   = "x-bkcrab-original-publish-attempt-id"
	rabbitDLQHeaderQueueTenantHash   = "x-bkcrab-queue-tenant-hash"
	rabbitDLQHeaderRawBodyOmitted    = "x-bkcrab-raw-body-omitted"
	rabbitDLQHeaderRawBodySize       = "x-bkcrab-raw-body-size"
	rabbitDLQHeaderRawBodySHA256     = "x-bkcrab-raw-body-sha256"
	rabbitNotificationBuffer         = 8
	rabbitDefaultDialTimeout         = 30 * time.Second
	rabbitDefaultHeartbeat           = 10 * time.Second
	rabbitDefaultOperationTimeout    = 15 * time.Second
	rabbitGracefulCloseTimeout       = time.Second
	rabbitDeliveryErrorInvalidBody   = "invalid-body"
	rabbitDeliveryErrorBodyTooLarge  = "body-too-large"
	rabbitDeliveryErrorRawTooLarge   = "raw-body-too-large"
	rabbitDeliveryErrorInvalidHeader = "invalid-headers"
	rabbitDeliveryErrorInvalidProps  = "invalid-properties"
)

var ErrDeliverySettled = errors.New("fairqueue: rabbit delivery is already settled")

type RabbitOptions struct {
	URL                string
	Exchange           string
	DeadLetterExchange string
	OperationTimeout   time.Duration
	Telemetry          TelemetrySink
}

// RabbitResourceProbe is a sanitized resource-level startup observation. It
// proves the shared exchanges and the registered resource DLQ can be declared
// and inspected without requiring a tenant or publishing a synthetic message.
type RabbitResourceProbe struct {
	Resource        string `json:"resource"`
	DeadLetterDepth int64  `json:"dead_letter_depth"`
}

type Rabbit struct {
	options   RabbitOptions
	registry  *Registry
	dialer    rabbitDialer
	attempts  rabbitAttemptIDSource
	healthNow func() time.Time
	telemetry TelemetrySink

	operationGate *rabbitOperationGate
	healthMu      sync.RWMutex
	health        RabbitHealthSnapshot

	stateMu        sync.Mutex
	closed         bool
	nextGeneration uint64
	generation     *rabbitGeneration
}

type rabbitDialer interface {
	Dial(context.Context, string) (rabbitConnection, error)
}

type rabbitConnection interface {
	OpenChannel() (rabbitChannel, error)
	CloseDeadline(time.Time) error
	Abort() error
}

type rabbitChannel interface {
	ExchangeDeclare(string, string, bool, bool, bool, bool, amqp.Table) error
	QueueDeclare(string, bool, bool, bool, bool, amqp.Table) (amqp.Queue, error)
	QueueBind(string, string, string, bool, amqp.Table) error
	QueueInspect(string) (amqp.Queue, error)
	Confirm(bool) error
	NotifyPublish(chan amqp.Confirmation) chan amqp.Confirmation
	NotifyReturn(chan amqp.Return) chan amqp.Return
	NotifyClose(chan *amqp.Error) chan *amqp.Error
	GetNextPublishSeqNo() uint64
	PublishWithContext(context.Context, string, string, bool, bool, amqp.Publishing) error
	Get(string, bool) (amqp.Delivery, bool, error)
	Close() error
}

type rabbitAttemptIDSource interface {
	Next() (string, error)
}

type rabbitGeneration struct {
	id         uint64
	connection rabbitConnection
	channel    rabbitChannel
	confirms   <-chan amqp.Confirmation
	returns    <-chan amqp.Return
	closes     <-chan *amqp.Error
	done       chan struct{}

	baseDeclared       bool
	tenantTopologies   map[string]struct{}
	deadLetterTopology map[string]struct{}

	deadOnce         sync.Once
	abortOnce        sync.Once
	boundedCloseOnce sync.Once
}

type rabbitOperationGate struct {
	token chan struct{}
}

type rabbitTenantTopology struct {
	resource   string
	tenantHash string
	queue      string
	routingKey string
}

type realRabbitDialer struct{}

type realRabbitConnection struct {
	connection *amqp.Connection
	raw        net.Conn
}

type cryptoRabbitAttemptIDs struct{}

type rabbitDelivery struct {
	request       PrepareRequest
	acknowledger  amqp.Acknowledger
	deliveryTag   uint64
	operationGate *rabbitOperationGate
	owner         *Rabbit
	generation    *rabbitGeneration

	mu      sync.Mutex
	settled bool
}

type deadLetterEnvelopeV1 struct {
	Version                  int               `json:"version"`
	RegisteredResource       string            `json:"registered_resource"`
	QueueTenantHash          string            `json:"queue_tenant_hash"`
	OriginalPublishAttemptID string            `json:"original_publish_attempt_id,omitempty"`
	ReasonCode               string            `json:"reason_code"`
	RawBody                  []byte            `json:"raw_body,omitempty"`
	RawBodyOmitted           bool              `json:"raw_body_omitted"`
	RawBodySize              int64             `json:"raw_body_size,omitempty"`
	RawBodySHA256            string            `json:"raw_body_sha256,omitempty"`
	BodyToken                *DispatchToken    `json:"body_token,omitempty"`
	HeaderToken              *DispatchToken    `json:"header_token,omitempty"`
	HeaderFacts              StableHeaderFacts `json:"header_facts"`
	BodyErrorCode            string            `json:"body_error_code,omitempty"`
	HeaderErrorCode          string            `json:"header_error_code,omitempty"`
	PropertyErrorCode        string            `json:"property_error_code,omitempty"`
}

func NewRabbit(options RabbitOptions, registry *Registry) (*Rabbit, error) {
	return newRabbit(options, registry, realRabbitDialer{}, cryptoRabbitAttemptIDs{})
}

func newRabbit(
	options RabbitOptions,
	registry *Registry,
	dialer rabbitDialer,
	attempts rabbitAttemptIDSource,
) (*Rabbit, error) {
	if strings.TrimSpace(options.URL) == "" || options.URL != strings.TrimSpace(options.URL) {
		return nil, errors.New("fairqueue: RabbitMQ URL is required without surrounding whitespace")
	}
	if registry == nil {
		return nil, errors.New("fairqueue: RabbitMQ resource registry is required")
	}
	if dialer == nil || attempts == nil {
		return nil, errors.New("fairqueue: RabbitMQ dependencies are required")
	}
	if options.Exchange == "" {
		options.Exchange = DefaultRabbitExchange
	}
	if options.DeadLetterExchange == "" {
		options.DeadLetterExchange = DefaultRabbitDeadLetterExchange
	}
	if options.OperationTimeout == 0 {
		options.OperationTimeout = rabbitDefaultOperationTimeout
	}
	if options.OperationTimeout < 0 || options.OperationTimeout > maxResourceDuration {
		return nil, errors.New("fairqueue: RabbitMQ operation timeout must be positive and bounded")
	}
	if err := validateRabbitEntityName("exchange", options.Exchange); err != nil {
		return nil, err
	}
	if err := validateRabbitEntityName("dead-letter exchange", options.DeadLetterExchange); err != nil {
		return nil, err
	}
	if options.Exchange == options.DeadLetterExchange {
		return nil, errors.New("fairqueue: RabbitMQ task and dead-letter exchanges must differ")
	}
	return &Rabbit{
		options: options, registry: registry, dialer: dialer, attempts: attempts,
		telemetry:     options.Telemetry,
		operationGate: newRabbitOperationGate(),
		healthNow:     time.Now,
		health:        RabbitHealthSnapshot{Status: DependencyStatusUnavailable},
	}, nil
}

func validateRabbitEntityName(kind, name string) error {
	if err := validateBoundedText("RabbitMQ "+kind, name, 255, false); err != nil {
		return err
	}
	return nil
}

func (realRabbitDialer) Dial(ctx context.Context, url string) (rabbitConnection, error) {
	if ctx == nil {
		return nil, errors.New("fairqueue: nil RabbitMQ dial context")
	}
	dialCtx := ctx
	cancel := func() {}
	if _, ok := ctx.Deadline(); !ok {
		dialCtx, cancel = context.WithTimeout(ctx, rabbitDefaultDialTimeout)
	}
	defer cancel()

	netDialer := &net.Dialer{}
	var rawConnection net.Conn
	config := amqp.Config{
		Heartbeat: rabbitDefaultHeartbeat,
		Locale:    "en_US",
		Dial: func(network, address string) (net.Conn, error) {
			connection, err := netDialer.DialContext(dialCtx, network, address)
			if err != nil {
				return nil, err
			}
			if deadline, ok := dialCtx.Deadline(); ok {
				if err := connection.SetDeadline(deadline); err != nil {
					_ = connection.Close()
					return nil, err
				}
			}
			rawConnection = connection
			return connection, nil
		},
	}
	connection, err := amqp.DialConfig(url, config)
	if err != nil {
		if rawConnection != nil {
			_ = rawConnection.Close()
		}
		return nil, err
	}
	return realRabbitConnection{connection: connection, raw: rawConnection}, nil
}

func (c realRabbitConnection) OpenChannel() (rabbitChannel, error) {
	return c.connection.Channel()
}

func (c realRabbitConnection) CloseDeadline(deadline time.Time) error {
	return c.connection.CloseDeadline(deadline)
}

func (c realRabbitConnection) Abort() error {
	if c.raw != nil {
		return c.raw.Close()
	}
	return c.connection.CloseDeadline(time.Now())
}

func (cryptoRabbitAttemptIDs) Next() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func newRabbitOperationGate() *rabbitOperationGate {
	gate := &rabbitOperationGate{token: make(chan struct{}, 1)}
	gate.token <- struct{}{}
	return gate
}

func (g *rabbitOperationGate) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.token:
	}
	if err := validateRabbitContext(ctx); err != nil {
		g.release()
		return err
	}
	return nil
}

func (g *rabbitOperationGate) release() {
	g.token <- struct{}{}
}

func (r *Rabbit) operationContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	if parent == nil {
		return nil, nil, errors.New("fairqueue: nil RabbitMQ context")
	}
	ctx, cancel := context.WithTimeout(parent, r.options.OperationTimeout)
	if err := validateRabbitContext(ctx); err != nil {
		cancel()
		return nil, nil, err
	}
	return ctx, cancel, nil
}

// HealthSnapshot returns the latest in-memory RabbitMQ observations. It never
// performs broker I/O and copies timestamp pointers so callers cannot mutate
// the transport's cached state.
func (r *Rabbit) HealthSnapshot() RabbitHealthSnapshot {
	if r == nil {
		return RabbitHealthSnapshot{Status: DependencyStatusUnavailable}
	}
	r.healthMu.RLock()
	snapshot := r.health
	snapshot.LastConfirmAt = cloneHealthTimePtr(r.health.LastConfirmAt)
	snapshot.LastReturnAt = cloneHealthTimePtr(r.health.LastReturnAt)
	r.healthMu.RUnlock()
	return snapshot
}

func (r *Rabbit) markRabbitOK(generation *rabbitGeneration) {
	if r == nil || generation == nil {
		return
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.closed || r.generation != generation || generation.isDead() {
		return
	}
	r.healthMu.Lock()
	r.health.Status = DependencyStatusOK
	r.healthMu.Unlock()
}

func (r *Rabbit) markRabbitUnavailable() {
	if r == nil {
		return
	}
	r.healthMu.Lock()
	r.health.Status = DependencyStatusUnavailable
	r.healthMu.Unlock()
}

func (r *Rabbit) markRabbitProbeOK(generation *rabbitGeneration, depth int64) {
	if r == nil || generation == nil {
		return
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.closed || r.generation != generation || generation.isDead() {
		return
	}
	r.healthMu.Lock()
	r.health.Status = DependencyStatusOK
	r.health.DLQDepthSample = depth
	r.healthMu.Unlock()
}

func (r *Rabbit) markRabbitReadyDepthOK(generation *rabbitGeneration, depth int64) {
	if r == nil || generation == nil {
		return
	}
	r.stateMu.Lock()
	defer r.stateMu.Unlock()
	if r.closed || r.generation != generation || generation.isDead() {
		return
	}
	r.healthMu.Lock()
	r.health.Status = DependencyStatusOK
	r.health.ReadyDepthSample = depth
	r.healthMu.Unlock()
}

func (r *Rabbit) markRabbitConfirm() {
	if r == nil {
		return
	}
	now := r.healthNow().UTC()
	r.healthMu.Lock()
	r.health.LastConfirmAt = cloneHealthTime(now)
	r.healthMu.Unlock()
}

func (r *Rabbit) markRabbitReturn() {
	if r == nil {
		return
	}
	now := r.healthNow().UTC()
	r.healthMu.Lock()
	r.health.LastReturnAt = cloneHealthTime(now)
	r.healthMu.Unlock()
}

// ProbeResourceTopology establishes one live confirm-capable generation,
// declares the durable shared exchanges and resource DLQ topology, and
// verifies the DLQ can be inspected. It deliberately has no tenant input, so
// an empty canonical database cannot bypass RabbitMQ startup readiness.
func (r *Rabbit) ProbeResourceTopology(ctx context.Context, resource string) (probe RabbitResourceProbe, err error) {
	if _, ok := r.registry.Lookup(resource); !ok {
		return RabbitResourceProbe{}, fmt.Errorf("fairqueue: unknown registered resource %q", resource)
	}
	defer func() {
		if err != nil {
			r.markRabbitUnavailable()
		}
	}()
	operationCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return RabbitResourceProbe{}, err
	}
	defer cancel()

	if err := r.operationGate.acquire(operationCtx); err != nil {
		return RabbitResourceProbe{}, rabbitDependencyContextError(err, "acquire resource probe operation gate")
	}
	defer r.operationGate.release()
	generation, err := r.ensureGeneration(operationCtx)
	if err != nil {
		return RabbitResourceProbe{}, err
	}
	if err := r.ensureDeadLetterTopology(operationCtx, generation, resource); err != nil {
		r.invalidateGeneration(generation, true)
		return RabbitResourceProbe{}, rabbitTopologyError(err, "declare resource probe topology")
	}
	queueName, err := DeadLetterQueueName(resource)
	if err != nil {
		return RabbitResourceProbe{}, err
	}
	queue, err := rabbitRPCValue1(r, operationCtx, generation, func() (amqp.Queue, error) {
		return generation.channel.QueueInspect(queueName)
	})
	if err != nil {
		r.invalidateGeneration(generation, true)
		return RabbitResourceProbe{}, rabbitDependencyContextError(err, "inspect resource dead-letter queue")
	}
	probe = RabbitResourceProbe{Resource: resource, DeadLetterDepth: int64(queue.Messages)}
	r.markRabbitProbeOK(generation, probe.DeadLetterDepth)
	return probe, nil
}

func (r *Rabbit) EnsureTenantTopology(ctx context.Context, resource, tenant string) (err error) {
	topology, err := r.validateTenantTopology(resource, tenant)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			r.markRabbitUnavailable()
		}
	}()
	operationCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	if err := r.operationGate.acquire(operationCtx); err != nil {
		return rabbitDependencyContextError(err, "acquire topology operation gate")
	}
	defer r.operationGate.release()
	generation, err := r.ensureGeneration(operationCtx)
	if err != nil {
		return err
	}
	if err := r.ensureTenantTopology(operationCtx, generation, topology); err != nil {
		r.invalidateGeneration(generation, true)
		return rabbitTopologyError(err, "declare tenant topology")
	}
	r.markRabbitOK(generation)
	return nil
}

func (r *Rabbit) PublishMandatoryConfirmed(ctx context.Context, message Message) (receipt PublishReceipt, err error) {
	if err := r.registry.ValidateMessage(message); err != nil {
		return PublishReceipt{}, err
	}
	body, err := json.Marshal(message)
	if err != nil {
		return PublishReceipt{}, err
	}
	topology, err := r.validateTenantTopology(message.Resource, message.TenantID)
	if err != nil {
		return PublishReceipt{}, err
	}
	defer func() {
		if err != nil {
			r.markRabbitUnavailable()
			EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryDependencyTransition, Resource: message.Resource, Outcome: "unavailable", Dependency: "rabbitmq"})
		}
	}()
	receipt, err = r.allocateAttempt()
	if err != nil {
		return PublishReceipt{}, err
	}
	operationCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return receipt, errors.Join(ErrPublishUnconfirmed, err)
	}
	defer cancel()

	publishing := amqp.Publishing{
		Headers: amqp.Table{
			rabbitHeaderProtocolVersion:    int32(MessageVersion1),
			rabbitHeaderResource:           message.Resource,
			rabbitHeaderTaskID:             message.TaskID,
			rabbitHeaderDispatchGeneration: int64(message.DispatchToken.Generation),
		},
		ContentType:   "application/json",
		DeliveryMode:  amqp.Persistent,
		CorrelationId: receipt.AttemptID,
		MessageId:     receipt.AttemptID,
		Type:          "bkcrab.fair.v1",
		AppId:         "bkcrab",
		Body:          body,
	}

	if err := r.operationGate.acquire(operationCtx); err != nil {
		return receipt, errors.Join(ErrPublishUnconfirmed, rabbitDependencyContextError(err, "acquire operation gate"))
	}
	defer r.operationGate.release()
	generation, err := r.ensureGeneration(operationCtx)
	if err != nil {
		return receipt, errors.Join(ErrPublishUnconfirmed, err)
	}
	if err := r.ensureTenantTopology(operationCtx, generation, topology); err != nil {
		r.invalidateGeneration(generation, true)
		return receipt, errors.Join(ErrPublishUnconfirmed, rabbitTopologyError(err, "declare publish topology"))
	}
	err = r.publishConfirmed(operationCtx, generation, r.options.Exchange, topology.routingKey, publishing, receipt.AttemptID)
	if errors.Is(err, ErrPublishUnroutable) {
		delete(generation.tenantTopologies, topology.queue)
	}
	if err == nil {
		r.markRabbitOK(generation)
		EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryDependencyTransition, Resource: message.Resource, Outcome: "ready", Dependency: "rabbitmq"})
	}
	return receipt, err
}

func (r *Rabbit) GetOne(ctx context.Context, resource, tenant string) (result Delivery, found bool, err error) {
	topology, err := r.validateTenantTopology(resource, tenant)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if err != nil {
			r.markRabbitUnavailable()
		}
	}()
	operationCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return nil, false, err
	}
	defer cancel()

	if err := r.operationGate.acquire(operationCtx); err != nil {
		return nil, false, rabbitDependencyContextError(err, "acquire consume operation gate")
	}
	defer r.operationGate.release()
	generation, err := r.ensureGeneration(operationCtx)
	if err != nil {
		return nil, false, err
	}
	if err := r.ensureTenantTopology(operationCtx, generation, topology); err != nil {
		r.invalidateGeneration(generation, true)
		return nil, false, rabbitTopologyError(err, "declare consume topology")
	}
	delivery, ok, err := rabbitGetRPC(r, operationCtx, generation, func() (amqp.Delivery, bool, error) {
		return generation.channel.Get(topology.queue, false)
	})
	if err != nil {
		r.invalidateGeneration(generation, true)
		return nil, false, rabbitDependencyContextError(err, "get delivery")
	}
	if !ok {
		r.markRabbitOK(generation)
		return nil, false, nil
	}
	request := r.prepareRequest(delivery, topology)
	if err := request.Validate(); err != nil {
		r.invalidateGeneration(generation, true)
		return nil, false, fmt.Errorf("%w: RabbitMQ delivery violated the transport contract", ErrInvalidModel)
	}
	result = &rabbitDelivery{
		request: request, acknowledger: delivery.Acknowledger, deliveryTag: delivery.DeliveryTag,
		operationGate: r.operationGate, owner: r, generation: generation,
	}
	r.markRabbitOK(generation)
	return result, true, nil
}

func (r *Rabbit) ReadyDepth(ctx context.Context, resource, tenant string) (depth int64, err error) {
	topology, err := r.validateTenantTopology(resource, tenant)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			r.markRabbitUnavailable()
			EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryDependencyTransition, Resource: resource, Outcome: "unavailable", Dependency: "rabbitmq"})
		}
	}()
	operationCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return 0, err
	}
	defer cancel()

	if err := r.operationGate.acquire(operationCtx); err != nil {
		return 0, rabbitDependencyContextError(err, "acquire depth operation gate")
	}
	defer r.operationGate.release()
	generation, err := r.ensureGeneration(operationCtx)
	if err != nil {
		return 0, err
	}
	if err := r.ensureTenantTopology(operationCtx, generation, topology); err != nil {
		r.invalidateGeneration(generation, true)
		return 0, rabbitTopologyError(err, "declare depth topology")
	}
	queue, err := rabbitRPCValue1(r, operationCtx, generation, func() (amqp.Queue, error) {
		return generation.channel.QueueInspect(topology.queue)
	})
	if err != nil {
		r.invalidateGeneration(generation, true)
		return 0, rabbitDependencyContextError(err, "inspect ready depth")
	}
	depth = int64(queue.Messages)
	r.markRabbitReadyDepthOK(generation, depth)
	EmitTelemetry(ctx, r.telemetry, TelemetryEvent{Name: TelemetryRabbitDepth, Resource: resource, Outcome: "ok", Dependency: "rabbitmq", Value: depth})
	return depth, nil
}

func (r *Rabbit) PublishDeadLetterConfirmed(ctx context.Context, request DeadLetterRequest) (receipt PublishReceipt, err error) {
	if err := request.Validate(); err != nil {
		return PublishReceipt{}, err
	}
	if _, ok := r.registry.Lookup(request.Delivery.RegisteredResource); !ok {
		return PublishReceipt{}, fmt.Errorf("fairqueue: unknown registered resource %q", request.Delivery.RegisteredResource)
	}
	envelope := makeDeadLetterEnvelope(request)
	body, err := json.Marshal(envelope)
	if err != nil {
		return PublishReceipt{}, fmt.Errorf("fairqueue: encode dead-letter envelope: %w", err)
	}
	defer func() {
		if err != nil {
			r.markRabbitUnavailable()
		}
	}()
	receipt, err = r.allocateAttempt()
	if err != nil {
		return PublishReceipt{}, err
	}
	operationCtx, cancel, err := r.operationContext(ctx)
	if err != nil {
		return receipt, errors.Join(ErrPublishUnconfirmed, err)
	}
	defer cancel()

	headers := amqp.Table{
		rabbitDLQHeaderReason:          request.ReasonCode,
		rabbitDLQHeaderQueueTenantHash: request.Delivery.QueueTenantHash,
		rabbitDLQHeaderRawBodyOmitted:  request.Delivery.RawBodyOmitted,
	}
	if request.Delivery.PublishAttemptID != "" {
		headers[rabbitDLQHeaderOriginalAttempt] = request.Delivery.PublishAttemptID
	}
	if request.Delivery.RawBodyOmitted {
		headers[rabbitDLQHeaderRawBodySize] = request.Delivery.RawBodySize
		headers[rabbitDLQHeaderRawBodySHA256] = request.Delivery.RawBodySHA256
	}
	publishing := amqp.Publishing{
		Headers:       headers,
		ContentType:   "application/json",
		DeliveryMode:  amqp.Persistent,
		CorrelationId: receipt.AttemptID,
		MessageId:     receipt.AttemptID,
		Type:          "bkcrab.fair.dlq.v1",
		AppId:         "bkcrab",
		Body:          body,
	}

	if err := r.operationGate.acquire(operationCtx); err != nil {
		return receipt, errors.Join(ErrPublishUnconfirmed, rabbitDependencyContextError(err, "acquire operation gate"))
	}
	defer r.operationGate.release()
	generation, err := r.ensureGeneration(operationCtx)
	if err != nil {
		return receipt, errors.Join(ErrPublishUnconfirmed, err)
	}
	resource := request.Delivery.RegisteredResource
	if err := r.ensureDeadLetterTopology(operationCtx, generation, resource); err != nil {
		r.invalidateGeneration(generation, true)
		return receipt, errors.Join(ErrPublishUnconfirmed, rabbitTopologyError(err, "declare dead-letter topology"))
	}
	err = r.publishConfirmed(operationCtx, generation, r.options.DeadLetterExchange, resource, publishing, receipt.AttemptID)
	if errors.Is(err, ErrPublishUnroutable) {
		delete(generation.deadLetterTopology, resource)
	}
	if err == nil {
		r.markRabbitOK(generation)
	}
	return receipt, err
}

func (r *Rabbit) Close() error {
	r.stateMu.Lock()
	if r.closed {
		r.stateMu.Unlock()
		r.markRabbitUnavailable()
		return nil
	}
	r.closed = true
	generation := r.generation
	r.generation = nil
	r.stateMu.Unlock()
	r.markRabbitUnavailable()
	if generation != nil {
		generation.markDead()
		generation.closeBounded()
	}
	return nil
}

func (r *Rabbit) allocateAttempt() (PublishReceipt, error) {
	attemptID, err := r.attempts.Next()
	if err != nil || !lowerHex32Pattern.MatchString(attemptID) {
		return PublishReceipt{}, rabbitDependencyError("allocate publish attempt")
	}
	return PublishReceipt{AttemptID: attemptID}, nil
}

func (r *Rabbit) validateTenantTopology(resource, tenant string) (rabbitTenantTopology, error) {
	if _, ok := r.registry.Lookup(resource); !ok {
		return rabbitTenantTopology{}, fmt.Errorf("fairqueue: unknown registered resource %q", resource)
	}
	hash, err := TenantHash(resource, tenant)
	if err != nil {
		return rabbitTenantTopology{}, err
	}
	queue, err := TenantQueueName(resource, tenant)
	if err != nil {
		return rabbitTenantTopology{}, err
	}
	routingKey, err := TenantRoutingKey(resource, tenant)
	if err != nil {
		return rabbitTenantTopology{}, err
	}
	return rabbitTenantTopology{resource: resource, tenantHash: hash, queue: queue, routingKey: routingKey}, nil
}

func (r *Rabbit) ensureGeneration(ctx context.Context) (*rabbitGeneration, error) {
	if err := validateRabbitContext(ctx); err != nil {
		return nil, err
	}
	r.stateMu.Lock()
	if r.closed {
		r.stateMu.Unlock()
		return nil, rabbitDependencyError("use closed client")
	}
	if r.generation != nil && !r.generation.isDead() {
		generation := r.generation
		r.stateMu.Unlock()
		return generation, nil
	}
	r.generation = nil
	r.stateMu.Unlock()

	connection, err := r.dial(ctx)
	if err != nil {
		return nil, rabbitDependencyContextError(err, "dial")
	}
	channel, err := openRabbitChannel(ctx, connection)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, rabbitDependencyContextError(err, "open channel")
		}
		_ = connection.Abort()
		return nil, rabbitDependencyError("open channel")
	}
	if err := setupRabbitConfirm(ctx, connection, channel); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, rabbitDependencyContextError(err, "enable publisher confirms")
		}
		_ = connection.Abort()
		return nil, rabbitDependencyError("enable publisher confirms")
	}
	confirms := channel.NotifyPublish(make(chan amqp.Confirmation, rabbitNotificationBuffer))
	returns := channel.NotifyReturn(make(chan amqp.Return, rabbitNotificationBuffer))
	closes := channel.NotifyClose(make(chan *amqp.Error, 1))

	r.stateMu.Lock()
	if r.closed {
		r.stateMu.Unlock()
		_ = connection.Abort()
		return nil, rabbitDependencyError("use closed client")
	}
	r.nextGeneration++
	generation := &rabbitGeneration{
		id: r.nextGeneration, connection: connection, channel: channel,
		confirms: confirms, returns: returns, closes: closes,
		done:             make(chan struct{}),
		tenantTopologies: make(map[string]struct{}), deadLetterTopology: make(map[string]struct{}),
	}
	r.generation = generation
	r.stateMu.Unlock()
	go r.watchGeneration(generation)
	return generation, nil
}

func (r *Rabbit) dial(ctx context.Context) (rabbitConnection, error) {
	type dialResult struct {
		connection rabbitConnection
		err        error
	}
	result := make(chan dialResult, 1)
	go func() {
		connection, err := r.dialer.Dial(ctx, r.options.URL)
		if ctx.Err() != nil && connection != nil {
			_ = connection.Abort()
		}
		result <- dialResult{connection: connection, err: err}
	}()
	select {
	case outcome := <-result:
		if err := validateRabbitContext(ctx); err != nil {
			if outcome.connection != nil {
				_ = outcome.connection.Abort()
			}
			return nil, err
		}
		if outcome.err != nil || outcome.connection == nil {
			return nil, rabbitDependencyError("dial")
		}
		return outcome.connection, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func openRabbitChannel(ctx context.Context, connection rabbitConnection) (rabbitChannel, error) {
	type channelResult struct {
		channel rabbitChannel
		err     error
	}
	result := make(chan channelResult, 1)
	go func() {
		channel, err := connection.OpenChannel()
		result <- channelResult{channel: channel, err: err}
	}()
	select {
	case outcome := <-result:
		if outcome.err == nil && outcome.channel == nil {
			return nil, rabbitDependencyError("open channel")
		}
		return outcome.channel, outcome.err
	case <-ctx.Done():
		_ = connection.Abort()
		return nil, ctx.Err()
	}
}

func setupRabbitConfirm(ctx context.Context, connection rabbitConnection, channel rabbitChannel) error {
	result := make(chan error, 1)
	go func() { result <- channel.Confirm(false) }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		_ = connection.Abort()
		return ctx.Err()
	}
}

func (r *Rabbit) ensureBaseTopology(ctx context.Context, generation *rabbitGeneration) error {
	if err := validateRabbitContext(ctx); err != nil {
		return err
	}
	if generation.isDead() {
		return rabbitDependencyError("use closed generation")
	}
	if generation.baseDeclared {
		return nil
	}
	if err := rabbitRPC(r, ctx, generation, func() error {
		return generation.channel.ExchangeDeclare(
			r.options.Exchange, "direct", true, false, false, false, nil,
		)
	}); err != nil {
		return err
	}
	if err := rabbitRPC(r, ctx, generation, func() error {
		return generation.channel.ExchangeDeclare(
			r.options.DeadLetterExchange, "direct", true, false, false, false, nil,
		)
	}); err != nil {
		return err
	}
	generation.baseDeclared = true
	return nil
}

func (r *Rabbit) ensureDeadLetterTopology(ctx context.Context, generation *rabbitGeneration, resource string) error {
	if err := validateRabbitContext(ctx); err != nil {
		return err
	}
	if generation.isDead() {
		return rabbitDependencyError("use closed generation")
	}
	if _, ok := generation.deadLetterTopology[resource]; ok {
		return nil
	}
	if err := r.ensureBaseTopology(ctx, generation); err != nil {
		return err
	}
	queueName, err := DeadLetterQueueName(resource)
	if err != nil {
		return err
	}
	if _, err := rabbitRPCValue1(r, ctx, generation, func() (amqp.Queue, error) {
		return generation.channel.QueueDeclare(queueName, true, false, false, false, nil)
	}); err != nil {
		return err
	}
	if err := rabbitRPC(r, ctx, generation, func() error {
		return generation.channel.QueueBind(
			queueName, resource, r.options.DeadLetterExchange, false, nil,
		)
	}); err != nil {
		return err
	}
	generation.deadLetterTopology[resource] = struct{}{}
	return nil
}

func (r *Rabbit) ensureTenantTopology(ctx context.Context, generation *rabbitGeneration, topology rabbitTenantTopology) error {
	if err := validateRabbitContext(ctx); err != nil {
		return err
	}
	if generation.isDead() {
		return rabbitDependencyError("use closed generation")
	}
	if _, ok := generation.tenantTopologies[topology.queue]; ok {
		return nil
	}
	if err := r.ensureDeadLetterTopology(ctx, generation, topology.resource); err != nil {
		return err
	}
	arguments := amqp.Table{
		"x-dead-letter-exchange":    r.options.DeadLetterExchange,
		"x-dead-letter-routing-key": topology.resource,
	}
	if _, err := rabbitRPCValue1(r, ctx, generation, func() (amqp.Queue, error) {
		return generation.channel.QueueDeclare(
			topology.queue, true, false, false, false, arguments,
		)
	}); err != nil {
		return err
	}
	if err := rabbitRPC(r, ctx, generation, func() error {
		return generation.channel.QueueBind(
			topology.queue, topology.routingKey, r.options.Exchange, false, nil,
		)
	}); err != nil {
		return err
	}
	generation.tenantTopologies[topology.queue] = struct{}{}
	return nil
}

func (r *Rabbit) publishConfirmed(
	ctx context.Context,
	generation *rabbitGeneration,
	exchange, routingKey string,
	publishing amqp.Publishing,
	attemptID string,
) error {
	sequence := generation.channel.GetNextPublishSeqNo()
	publishResult := make(chan error, 1)
	go func() {
		publishResult <- generation.channel.PublishWithContext(
			ctx, exchange, routingKey, true, false, publishing,
		)
	}()

	select {
	case err := <-publishResult:
		if err != nil {
			r.invalidateGeneration(generation, true)
			return errors.Join(ErrPublishUnconfirmed, rabbitDependencyError("publish"))
		}
	case <-ctx.Done():
		r.invalidateGeneration(generation, true)
		return errors.Join(ErrPublishUnconfirmed, ErrDependencyUnavailable, ctx.Err())
	case <-generation.done:
		r.invalidateGeneration(generation, true)
		return errors.Join(ErrPublishUnconfirmed, rabbitDependencyError("channel closed during publish"))
	}

	returned := false
	for {
		select {
		case confirmation, ok := <-generation.confirms:
			if !ok || confirmation.DeliveryTag != sequence {
				r.invalidateGeneration(generation, true)
				return errors.Join(ErrPublishUnconfirmed, rabbitDependencyError("invalid publisher confirmation"))
			}
			r.markRabbitConfirm()
			drainedReturn, err := drainRabbitReturns(generation.returns, attemptID)
			if err != nil {
				r.invalidateGeneration(generation, true)
				return errors.Join(ErrPublishUnconfirmed, err)
			}
			returned = returned || drainedReturn
			if drainedReturn {
				r.markRabbitReturn()
			}
			if !confirmation.Ack {
				r.invalidateGeneration(generation, true)
				if returned {
					return errors.Join(ErrPublishUnconfirmed, ErrPublishUnroutable)
				}
				return ErrPublishUnconfirmed
			}
			if returned {
				return ErrPublishUnroutable
			}
			return nil
		case returnedMessage, ok := <-generation.returns:
			if !ok || !rabbitReturnMatches(returnedMessage, attemptID) {
				r.invalidateGeneration(generation, true)
				return errors.Join(ErrPublishUnconfirmed, rabbitDependencyError("invalid mandatory return"))
			}
			r.markRabbitReturn()
			returned = true
		case <-ctx.Done():
			r.invalidateGeneration(generation, true)
			return errors.Join(ErrPublishUnconfirmed, ErrDependencyUnavailable, ctx.Err())
		case <-generation.done:
			r.invalidateGeneration(generation, true)
			return errors.Join(ErrPublishUnconfirmed, rabbitDependencyError("channel closed before confirmation"))
		}
	}
}

func drainRabbitReturns(returns <-chan amqp.Return, attemptID string) (bool, error) {
	returned := false
	for {
		select {
		case returnedMessage, ok := <-returns:
			if !ok || !rabbitReturnMatches(returnedMessage, attemptID) {
				return false, rabbitDependencyError("invalid mandatory return ordering")
			}
			returned = true
		default:
			return returned, nil
		}
	}
}

func rabbitReturnMatches(returned amqp.Return, attemptID string) bool {
	return returned.MessageId == attemptID && returned.CorrelationId == attemptID
}

func (r *Rabbit) invalidateGeneration(generation *rabbitGeneration, force bool) {
	r.stateMu.Lock()
	if r.generation == generation {
		r.generation = nil
	}
	r.stateMu.Unlock()
	r.markRabbitUnavailable()
	generation.markDead()
	if force {
		generation.abort()
	} else {
		generation.closeBounded()
	}
}

func (r *Rabbit) watchGeneration(generation *rabbitGeneration) {
	select {
	case <-generation.closes:
		r.stateMu.Lock()
		if r.generation == generation {
			r.generation = nil
		}
		r.stateMu.Unlock()
		generation.abort()
		r.markRabbitUnavailable()
	case <-generation.done:
	}
}

func (g *rabbitGeneration) markDead() {
	g.deadOnce.Do(func() { close(g.done) })
}

func (g *rabbitGeneration) isDead() bool {
	select {
	case <-g.done:
		return true
	default:
		return false
	}
}

func (g *rabbitGeneration) abort() {
	g.markDead()
	g.abortOnce.Do(func() {
		if g.connection != nil {
			_ = g.connection.Abort()
			return
		}
		if g.channel != nil {
			_ = g.channel.Close()
		}
	})
}

func (g *rabbitGeneration) closeBounded() {
	g.markDead()
	g.boundedCloseOnce.Do(func() {
		if g.connection == nil {
			g.abort()
			return
		}
		deadline := time.Now().Add(rabbitGracefulCloseTimeout)
		closed := make(chan error, 1)
		go func() {
			closed <- g.connection.CloseDeadline(deadline)
		}()
		timer := time.NewTimer(rabbitGracefulCloseTimeout)
		defer timer.Stop()
		select {
		case err := <-closed:
			if err != nil {
				g.abort()
			}
		case <-timer.C:
			g.abort()
		}
	})
}

func rabbitRPC(r *Rabbit, ctx context.Context, generation *rabbitGeneration, call func() error) error {
	result := make(chan error, 1)
	go func() { result <- call() }()
	select {
	case err := <-result:
		if err == nil && generation.isDead() {
			return rabbitDependencyError("generation closed during RPC")
		}
		return err
	case <-ctx.Done():
		r.invalidateGeneration(generation, true)
		return ctx.Err()
	}
}

func rabbitRPCValue1[T any](
	r *Rabbit,
	ctx context.Context,
	generation *rabbitGeneration,
	call func() (T, error),
) (T, error) {
	type rpcResult struct {
		value T
		err   error
	}
	result := make(chan rpcResult, 1)
	go func() {
		value, err := call()
		result <- rpcResult{value: value, err: err}
	}()
	select {
	case outcome := <-result:
		if outcome.err == nil && generation.isDead() {
			var zero T
			return zero, rabbitDependencyError("generation closed during RPC")
		}
		return outcome.value, outcome.err
	case <-ctx.Done():
		r.invalidateGeneration(generation, true)
		var zero T
		return zero, ctx.Err()
	}
}

func rabbitGetRPC(
	r *Rabbit,
	ctx context.Context,
	generation *rabbitGeneration,
	call func() (amqp.Delivery, bool, error),
) (amqp.Delivery, bool, error) {
	type getResult struct {
		delivery amqp.Delivery
		ok       bool
		err      error
	}
	result := make(chan getResult, 1)
	go func() {
		delivery, ok, err := call()
		result <- getResult{delivery: delivery, ok: ok, err: err}
	}()
	select {
	case outcome := <-result:
		if outcome.err == nil && generation.isDead() {
			return amqp.Delivery{}, false, rabbitDependencyError("generation closed during get")
		}
		return outcome.delivery, outcome.ok, outcome.err
	case <-ctx.Done():
		r.invalidateGeneration(generation, true)
		return amqp.Delivery{}, false, ctx.Err()
	}
}

func rabbitTopologyError(err error, operation string) error {
	var amqpError *amqp.Error
	if errors.As(err, &amqpError) && amqpError.Code == amqp.PreconditionFailed {
		return ErrUnsupportedTopology
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(ErrDependencyUnavailable, err)
	}
	return rabbitDependencyError(operation)
}

func (r *Rabbit) prepareRequest(delivery amqp.Delivery, topology rabbitTenantTopology) PrepareRequest {
	request := PrepareRequest{
		RegisteredResource: topology.resource,
		QueueTenantHash:    topology.tenantHash,
	}
	if len(delivery.Body) > MaxRawDeliveryBytes {
		digest := sha256.Sum256(delivery.Body)
		request.RawBodyOmitted = true
		request.RawBodySize = int64(len(delivery.Body))
		request.RawBodySHA256 = hex.EncodeToString(digest[:])
		request.DecodeErrorCode = rabbitDeliveryErrorRawTooLarge
	} else {
		request.RawBody = append([]byte(nil), delivery.Body...)
		request.BodyCandidate, request.DecodeErrorCode = r.parseBodyCandidate(request.RawBody)
	}
	request.HeaderFacts, request.HeaderToken, request.HeaderErrorCode = r.parseHeaderFacts(delivery.Headers)
	request.PublishAttemptID, request.PropertyErrorCode = parseRabbitAttemptProperties(
		delivery.MessageId, delivery.CorrelationId,
	)

	if request.BodyCandidate != nil && request.HeaderToken != nil &&
		request.DecodeErrorCode == "" && request.HeaderErrorCode == "" && request.PropertyErrorCode == "" {
		completeHeader, complete := request.HeaderFacts.CompleteV1Token()
		tenantHash, hashErr := TenantHash(request.BodyCandidate.Resource, request.BodyCandidate.TenantID)
		if complete && completeHeader == *request.HeaderToken &&
			request.BodyCandidate.DispatchToken == *request.HeaderToken &&
			request.BodyCandidate.Resource == topology.resource && hashErr == nil && tenantHash == topology.tenantHash {
			message := *request.BodyCandidate
			request.Message = &message
		}
	}
	return request
}

func (r *Rabbit) parseBodyCandidate(raw []byte) (*Message, string) {
	if len(raw) > MaxMessageBytes {
		return nil, rabbitDeliveryErrorBodyTooLarge
	}
	message, err := StrictDecodeMessage(raw)
	if err != nil {
		return nil, rabbitDeliveryErrorInvalidBody
	}
	config, ok := r.registry.Lookup(message.Resource)
	if !ok {
		return nil, "unregistered-body-resource"
	}
	if !config.ValidateTaskID(message.TaskID) {
		return nil, "invalid-body-task-id"
	}
	if err := r.registry.ValidateMessage(message); err != nil {
		return nil, rabbitDeliveryErrorInvalidBody
	}
	return &message, ""
}

func (r *Rabbit) parseHeaderFacts(headers amqp.Table) (StableHeaderFacts, *DispatchToken, string) {
	var facts StableHeaderFacts
	invalid := false
	if value, ok := headers[rabbitHeaderProtocolVersion].(int32); ok {
		facts.ProtocolVersion = &value
		invalid = invalid || value != int32(MessageVersion1)
	} else {
		invalid = true
	}
	if value, ok := boundedRabbitStringHeader(headers, rabbitHeaderResource, maxResourceBytes); ok {
		facts.Resource = &value
	} else {
		invalid = true
	}
	if value, ok := boundedRabbitStringHeader(headers, rabbitHeaderTaskID, maxTaskIDBytes); ok {
		facts.TaskID = &value
	} else {
		invalid = true
	}
	if value, ok := headers[rabbitHeaderDispatchGeneration].(int64); ok {
		facts.DispatchGeneration = &value
		invalid = invalid || value <= 0
	} else {
		invalid = true
	}

	token, tokenOK := facts.Token()
	if !tokenOK {
		return facts, nil, rabbitDeliveryErrorInvalidHeader
	}
	config, registered := r.registry.Lookup(token.Resource)
	if !registered {
		return facts, nil, "unregistered-header-resource"
	}
	if !config.ValidateTaskID(token.TaskID) {
		return facts, nil, "invalid-header-task-id"
	}
	if invalid {
		return facts, &token, rabbitDeliveryErrorInvalidHeader
	}
	return facts, &token, ""
}

func boundedRabbitStringHeader(headers amqp.Table, key string, maxBytes int) (string, bool) {
	value, ok := headers[key].(string)
	if !ok {
		return "", false
	}
	if err := validateBoundedText("RabbitMQ header", value, maxBytes, false); err != nil {
		return "", false
	}
	return value, true
}

func parseRabbitAttemptProperties(messageID, correlationID string) (string, string) {
	if messageID == correlationID && lowerHex32Pattern.MatchString(messageID) {
		return messageID, ""
	}
	return "", rabbitDeliveryErrorInvalidProps
}

func makeDeadLetterEnvelope(request DeadLetterRequest) deadLetterEnvelopeV1 {
	delivery := request.Delivery
	var bodyToken *DispatchToken
	if delivery.BodyCandidate != nil {
		token := delivery.BodyCandidate.DispatchToken
		bodyToken = &token
	}
	var headerToken *DispatchToken
	if delivery.HeaderToken != nil {
		token := *delivery.HeaderToken
		headerToken = &token
	}
	return deadLetterEnvelopeV1{
		Version:                  MessageVersion1,
		RegisteredResource:       delivery.RegisteredResource,
		QueueTenantHash:          delivery.QueueTenantHash,
		OriginalPublishAttemptID: delivery.PublishAttemptID,
		ReasonCode:               request.ReasonCode,
		RawBody:                  append([]byte(nil), delivery.RawBody...),
		RawBodyOmitted:           delivery.RawBodyOmitted,
		RawBodySize:              delivery.RawBodySize,
		RawBodySHA256:            delivery.RawBodySHA256,
		BodyToken:                bodyToken,
		HeaderToken:              headerToken,
		HeaderFacts:              cloneStableHeaderFacts(delivery.HeaderFacts),
		BodyErrorCode:            delivery.DecodeErrorCode,
		HeaderErrorCode:          delivery.HeaderErrorCode,
		PropertyErrorCode:        delivery.PropertyErrorCode,
	}
}

func (d *rabbitDelivery) Request() PrepareRequest {
	return clonePrepareRequest(d.request)
}

func (d *rabbitDelivery) Ack(ctx context.Context) error {
	return d.settle(ctx, true, false)
}

func (d *rabbitDelivery) Nack(ctx context.Context, requeue bool) error {
	return d.settle(ctx, false, requeue)
}

func (d *rabbitDelivery) settle(ctx context.Context, ack, requeue bool) error {
	operationCtx := ctx
	cancel := func() {}
	var err error
	if d.owner != nil {
		operationCtx, cancel, err = d.owner.operationContext(ctx)
		if err != nil {
			return err
		}
	} else if err := validateRabbitContext(ctx); err != nil {
		return err
	}
	defer cancel()
	if d.operationGate != nil {
		if err := d.operationGate.acquire(operationCtx); err != nil {
			d.mu.Lock()
			if d.settled {
				d.mu.Unlock()
				return ErrDeliverySettled
			}
			d.settled = true
			d.mu.Unlock()
			d.abortGeneration()
			return rabbitDependencyContextError(err, "acquire settlement operation gate")
		}
		defer d.operationGate.release()
	}
	d.mu.Lock()
	if d.settled {
		d.mu.Unlock()
		return ErrDeliverySettled
	}
	d.settled = true
	d.mu.Unlock()
	if d.acknowledger == nil {
		d.abortGeneration()
		return rabbitDependencyError("settle delivery without acknowledger")
	}
	result := make(chan error, 1)
	go func() {
		if ack {
			result <- d.acknowledger.Ack(d.deliveryTag, false)
			return
		}
		result <- d.acknowledger.Nack(d.deliveryTag, false, requeue)
	}()
	select {
	case err := <-result:
		if err == nil && d.generation != nil && d.generation.isDead() {
			if d.owner != nil {
				d.owner.markRabbitUnavailable()
			}
			return rabbitDependencyError("generation closed during settlement")
		}
		if err != nil {
			d.abortGeneration()
			return rabbitDependencyError("settle delivery")
		}
		if d.owner != nil {
			d.owner.markRabbitOK(d.generation)
		}
		return nil
	case <-operationCtx.Done():
		d.abortGeneration()
		return errors.Join(ErrDependencyUnavailable, operationCtx.Err())
	case <-d.generationDone():
		d.abortGeneration()
		return rabbitDependencyError("generation closed during settlement")
	}
}

func (d *rabbitDelivery) generationDone() <-chan struct{} {
	if d.generation != nil {
		return d.generation.done
	}
	return nil
}

func (d *rabbitDelivery) abortGeneration() {
	if d.owner != nil && d.generation != nil {
		d.owner.invalidateGeneration(d.generation, true)
		return
	}
	if d.generation != nil {
		d.generation.abort()
	}
}

func clonePrepareRequest(request PrepareRequest) PrepareRequest {
	clone := request
	clone.RawBody = append([]byte(nil), request.RawBody...)
	clone.HeaderFacts = cloneStableHeaderFacts(request.HeaderFacts)
	if request.Message != nil {
		message := *request.Message
		clone.Message = &message
	}
	if request.BodyCandidate != nil {
		message := *request.BodyCandidate
		clone.BodyCandidate = &message
	}
	if request.HeaderToken != nil {
		token := *request.HeaderToken
		clone.HeaderToken = &token
	}
	return clone
}

func cloneStableHeaderFacts(facts StableHeaderFacts) StableHeaderFacts {
	clone := facts
	if facts.ProtocolVersion != nil {
		value := *facts.ProtocolVersion
		clone.ProtocolVersion = &value
	}
	if facts.Resource != nil {
		value := *facts.Resource
		clone.Resource = &value
	}
	if facts.TaskID != nil {
		value := *facts.TaskID
		clone.TaskID = &value
	}
	if facts.DispatchGeneration != nil {
		value := *facts.DispatchGeneration
		clone.DispatchGeneration = &value
	}
	return clone
}

func validateRabbitContext(ctx context.Context) error {
	if ctx == nil {
		return errors.New("fairqueue: nil RabbitMQ context")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func rabbitDependencyError(operation string) error {
	return fmt.Errorf("%w: RabbitMQ %s failed", ErrDependencyUnavailable, operation)
}

func rabbitDependencyContextError(err error, operation string) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return errors.Join(rabbitDependencyError(operation), err)
	}
	if errors.Is(err, ErrDependencyUnavailable) || errors.Is(err, ErrUnsupportedTopology) {
		return err
	}
	return rabbitDependencyError(operation)
}

var _ RabbitClient = (*Rabbit)(nil)
var _ Delivery = (*rabbitDelivery)(nil)
