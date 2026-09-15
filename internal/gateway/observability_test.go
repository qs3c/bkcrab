package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/observability"
	ragtelemetry "github.com/qs3c/bkcrab/internal/rag/telemetry"
)

func TestOperationalTelemetry(t *testing.T) {
	m := observability.New()
	observability.SetDefault(m)
	defer observability.SetDefault(nil)
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer slog.SetDefault(previous)
	sink := newFairQueueTelemetry(nil)
	for _, value := range []int64{8, 3} {
		sink.RecordFairQueue(context.Background(), fairqueue.TelemetryEvent{Name: fairqueue.TelemetryRabbitDepth, Resource: "rag.index", Outcome: "ok", Value: value, TaskID: "private-task"})
	}
	sink.RecordFairQueue(context.Background(), fairqueue.TelemetryEvent{Name: fairqueue.TelemetryRabbitDepth, Resource: "rag.index", Outcome: "error"})
	if n := testutil.ToFloat64(m.Samples.WithLabelValues("rag.index", "rabbit.ready_depth", "")); n != 3 {
		t.Fatalf("sample is %v, want 3", n)
	}
	sink.RecordFairQueue(context.Background(), fairqueue.TelemetryEvent{Name: fairqueue.TelemetryTaskRun, Resource: "image.generate", Outcome: "completed", Duration: time.Second})
	r := newRAGTelemetry()
	r.Record(context.Background(), ragtelemetry.Event{Name: ragtelemetry.EventResultCache, Fields: ragtelemetry.Fields{CacheKind: "parse_artifact", CacheStatus: "hit", DocID: "private-doc", Model: "private-model"}})
	r.Record(context.Background(), ragtelemetry.Event{Name: "private-signal"})
	if n := testutil.ToFloat64(m.Events.WithLabelValues("rag", "rag.result_cache", "parse_artifact", "hit")); n != 1 {
		t.Fatal(n)
	}
	w := httptest.NewRecorder()
	promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}).ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(w.Body.String(), "private-") {
		t.Fatal("sensitive or unbounded labels exported")
	}
}

func TestDisabledQueueDoesNotReportHealthyDependencies(t *testing.T) {
	m := observability.New()
	g := &Gateway{fairQueueHealth: newRAGFairQueueHealthState(ragFairQueueHealthOptions{Enabled: false})}
	g.RegisterOperationalCollectors(m)
	families, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range families {
		if f.GetName() == "bkcrab_fairqueue_enabled" {
			found = true
			if len(f.Metric) != 1 || f.Metric[0].GetGauge().GetValue() != 0 {
				t.Fatal(f)
			}
		}
		if f.GetName() == "bkcrab_fairqueue_dependency_up" || f.GetName() == "bkcrab_fairqueue_healthy" {
			t.Fatal("disabled queue reports dependency health")
		}
	}
	if !found {
		t.Fatal("missing enabled state")
	}
}
