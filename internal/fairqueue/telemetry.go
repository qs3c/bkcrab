package fairqueue

import (
	"context"
	"strings"
	"time"
)

// TelemetryName is a closed set of operational signals. Implementations may
// translate these events to counters, histograms, gauges, or structured logs.
// Tenant, task, token, URL, and credential values are intentionally not part
// of MetricLabels.
type TelemetryName string

const (
	TelemetryDispatchScan         TelemetryName = "dispatch.scan"
	TelemetryDispatchPublish      TelemetryName = "dispatch.publish"
	TelemetryDispatchMark         TelemetryName = "dispatch.mark"
	TelemetryRabbitDepth          TelemetryName = "rabbit.ready_depth"
	TelemetryTaskQueueWait        TelemetryName = "task.queue_wait"
	TelemetryTaskClaim            TelemetryName = "task.claim"
	TelemetryTaskRun              TelemetryName = "task.run"
	TelemetryExpiredRunning       TelemetryName = "task.expired_running"
	TelemetryActiveTenants        TelemetryName = "redis.active"
	TelemetryRing                 TelemetryName = "redis.ring"
	TelemetryRingMembers          TelemetryName = "redis.ring_members"
	TelemetryGlobalInflight       TelemetryName = "redis.inflight"
	TelemetryProcessingTurn       TelemetryName = "redis.processing_turn"
	TelemetryReservation          TelemetryName = "redis.reservation"
	TelemetryClaimLock            TelemetryName = "mysql.claim_lock"
	TelemetryClaimCapacity        TelemetryName = "mysql.claim_capacity"
	TelemetryHeartbeat            TelemetryName = "mysql.heartbeat"
	TelemetryDeadLetter           TelemetryName = "rabbit.dead_letter"
	TelemetryPrepareDisposition   TelemetryName = "task.prepare"
	TelemetryResourceState        TelemetryName = "resource.state"
	TelemetrySchedulerGate        TelemetryName = "scheduler.gate"
	TelemetryRecovery             TelemetryName = "recovery.run"
	TelemetryRecoveryPage         TelemetryName = "recovery.page"
	TelemetryCanonicalCorrection  TelemetryName = "recovery.correction"
	TelemetryDependencyTransition TelemetryName = "dependency.transition"
)

var telemetryNames = map[TelemetryName]struct{}{
	TelemetryDispatchScan: {}, TelemetryDispatchPublish: {}, TelemetryDispatchMark: {},
	TelemetryRabbitDepth: {}, TelemetryTaskQueueWait: {}, TelemetryTaskClaim: {},
	TelemetryTaskRun: {}, TelemetryExpiredRunning: {}, TelemetryActiveTenants: {},
	TelemetryRing: {}, TelemetryRingMembers: {}, TelemetryGlobalInflight: {},
	TelemetryProcessingTurn: {}, TelemetryReservation: {}, TelemetryClaimLock: {},
	TelemetryClaimCapacity: {}, TelemetryHeartbeat: {}, TelemetryDeadLetter: {},
	TelemetryPrepareDisposition: {}, TelemetryResourceState: {}, TelemetrySchedulerGate: {},
	TelemetryRecovery: {}, TelemetryRecoveryPage: {}, TelemetryCanonicalCorrection: {},
	TelemetryDependencyTransition: {},
}

var telemetryOutcomes = map[string]struct{}{
	"ok": {}, "error": {}, "timeout": {}, "returned": {}, "confirmed": {},
	"stale": {}, "granted": {}, "denied": {}, "renewed": {}, "released": {},
	"promoted": {}, "armed": {}, "redispatched": {}, "paused": {}, "ready": {},
	"recovering": {}, "fence_lost": {}, "mismatch": {}, "duplicate": {},
	"repair": {}, "dlq": {}, "requeue": {}, "empty": {}, "unavailable": {},
	"claimed": {}, "capacity_deferred": {}, "terminal": {}, "retry_not_due": {},
	"poison": {}, "canceled": {}, "invalid": {}, "started": {}, "completed": {},
}

var telemetryReservationKinds = map[string]struct{}{
	"": {}, "provisional": {}, "stable": {}, "processing": {},
}

var telemetryDependencies = map[string]struct{}{
	"": {}, "mysql": {}, "redis": {}, "rabbitmq": {}, "runtime": {},
}

// TelemetryEvent has a closed metric-label surface and a separate bounded log
// correlation surface. Correlation fields must never be promoted to metric
// labels by a sink.
type TelemetryEvent struct {
	Name            TelemetryName
	Resource        string
	Outcome         string
	ReservationKind string
	Dependency      string
	Value           int64
	Duration        time.Duration

	// Log-only correlation fields. TenantHash is a one-way hash produced by
	// the queue protocol; raw tenant IDs are never accepted here.
	TaskID             string
	DispatchGeneration int64
	PublishAttemptID   string
	TenantHash         string
}

// MetricLabels returns the complete set of dimensions a metrics exporter may
// use. Its fixed keys make accidental high-cardinality exports testable.
func (e TelemetryEvent) MetricLabels() map[string]string {
	return map[string]string{
		"resource":         e.Resource,
		"outcome":          e.Outcome,
		"reservation_kind": e.ReservationKind,
		"dependency":       e.Dependency,
	}
}

// TelemetrySink is deliberately non-failing. EmitTelemetry also contains a
// panic boundary so an exporter can never change claim, ACK, or fence results.
type TelemetrySink interface {
	RecordFairQueue(context.Context, TelemetryEvent)
}

type TelemetrySinkFunc func(context.Context, TelemetryEvent)

func (f TelemetrySinkFunc) RecordFairQueue(ctx context.Context, event TelemetryEvent) {
	if f != nil {
		f(ctx, event)
	}
}

type nopTelemetrySink struct{}

func (nopTelemetrySink) RecordFairQueue(context.Context, TelemetryEvent) {}

func NopTelemetrySink() TelemetrySink { return nopTelemetrySink{} }

// EmitTelemetry validates and sanitizes the closed event before delivery.
// Invalid events are dropped rather than converted into unbounded labels.
func EmitTelemetry(ctx context.Context, sink TelemetrySink, event TelemetryEvent) {
	if sink == nil || ctx == nil {
		return
	}
	if _, ok := telemetryNames[event.Name]; !ok {
		return
	}
	if ValidateResource(event.Resource) != nil {
		return
	}
	if _, ok := telemetryOutcomes[event.Outcome]; !ok {
		return
	}
	if _, ok := telemetryReservationKinds[event.ReservationKind]; !ok {
		return
	}
	if _, ok := telemetryDependencies[event.Dependency]; !ok {
		return
	}
	if event.Value < 0 {
		event.Value = 0
	}
	if event.Duration < 0 {
		event.Duration = 0
	}
	event.TaskID = boundedTelemetryToken(event.TaskID, 128)
	event.PublishAttemptID = boundedTelemetryHex(event.PublishAttemptID, 32)
	event.TenantHash = boundedTelemetryHex(event.TenantHash, 64)
	if event.DispatchGeneration < 0 {
		event.DispatchGeneration = 0
	}
	defer func() { _ = recover() }()
	sink.RecordFairQueue(ctx, event)
}

func boundedTelemetryToken(value string, maximum int) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximum {
		return ""
	}
	for _, current := range value {
		if current < 0x21 || current > 0x7e || current == '/' || current == '\\' || current == '?' || current == '&' || current == '=' {
			return ""
		}
	}
	return value
}

func boundedTelemetryHex(value string, length int) string {
	value = strings.TrimSpace(value)
	if len(value) != length {
		return ""
	}
	for _, current := range value {
		if !((current >= '0' && current <= '9') || (current >= 'a' && current <= 'f')) {
			return ""
		}
	}
	return value
}
