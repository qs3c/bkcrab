package fairqueue

import (
	"context"
	"reflect"
	"testing"
)

func TestTelemetryMetricLabelsStayLowCardinality(t *testing.T) {
	event := TelemetryEvent{
		Name: TelemetryDispatchPublish, Resource: "rag.index", Outcome: "confirmed",
		Dependency: "rabbitmq", TaskID: "ragi_123", DispatchGeneration: 7,
		PublishAttemptID: "0123456789abcdef0123456789abcdef",
		TenantHash:       "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	}
	want := map[string]string{
		"resource": "rag.index", "outcome": "confirmed", "reservation_kind": "", "dependency": "rabbitmq",
	}
	if got := event.MetricLabels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("MetricLabels()=%v, want %v", got, want)
	}
}

func TestTelemetryDropsInvalidDimensionsAndCannotBreakQueue(t *testing.T) {
	var events []TelemetryEvent
	sink := TelemetrySinkFunc(func(_ context.Context, event TelemetryEvent) { events = append(events, event) })
	EmitTelemetry(context.Background(), sink, TelemetryEvent{Name: "arbitrary", Resource: "rag.index", Outcome: "ok"})
	EmitTelemetry(context.Background(), sink, TelemetryEvent{Name: TelemetryDispatchScan, Resource: "rag.index", Outcome: "secret-user"})
	EmitTelemetry(context.Background(), sink, TelemetryEvent{
		Name: TelemetryDispatchScan, Resource: "rag.index", Outcome: "ok", Value: -1,
		TaskID: "https://user:secret@example.test/task?a=b", TenantHash: "raw-tenant",
	})
	if len(events) != 1 {
		t.Fatalf("events=%d, want 1", len(events))
	}
	if events[0].Value != 0 || events[0].TaskID != "" || events[0].TenantHash != "" {
		t.Fatalf("event was not sanitized: %#v", events[0])
	}

	panicking := TelemetrySinkFunc(func(context.Context, TelemetryEvent) { panic("exporter failed") })
	EmitTelemetry(context.Background(), panicking, TelemetryEvent{Name: TelemetryDispatchScan, Resource: "rag.index", Outcome: "ok"})
}
