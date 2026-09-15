package gateway

import (
	"context"
	"database/sql"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/observability"
	ragtelemetry "github.com/qs3c/bkcrab/internal/rag/telemetry"
)

// newRAGTelemetry preserves existing logs and sends only sanitized, closed
// dimensions to Prometheus. IDs, versions, providers and models stay out.
func newRAGTelemetry() ragtelemetry.Recorder {
	logger := ragtelemetry.NewSlogRecorder(nil)
	m := observability.Current()
	if m == nil {
		return logger
	}
	return ragtelemetry.RecorderFunc(func(ctx context.Context, event ragtelemetry.Event) {
		ragtelemetry.Emit(ctx, ragtelemetry.RecorderFunc(func(ctx context.Context, e ragtelemetry.Event) {
			logger.Record(ctx, e)
			if e.Name == ragtelemetry.EventFairQueue {
				return
			}
			op, outcome := e.Fields.Operation, e.Fields.Outcome
			if op == "" {
				op = e.Fields.Transition
			}
			if e.Name == ragtelemetry.EventResultCache {
				op, outcome = e.Fields.CacheKind, e.Fields.CacheStatus
			}
			m.RecordEvent("rag", string(e.Name), op, outcome, e.Fields.Duration)
		}), event.Name, event.Fields)
	})
}

func newFairQueueTelemetry(logs fairqueue.TelemetrySink) fairqueue.TelemetrySink {
	m := observability.Current()
	if m == nil {
		if logs != nil {
			return logs
		}
		return fairqueue.NopTelemetrySink()
	}
	return fairqueue.TelemetrySinkFunc(func(ctx context.Context, event fairqueue.TelemetryEvent) {
		fairqueue.EmitTelemetry(ctx, fairqueue.TelemetrySinkFunc(func(ctx context.Context, e fairqueue.TelemetryEvent) {
			if logs != nil {
				logs.RecordFairQueue(ctx, e)
			}
			if m == nil || (e.Resource != "rag.index" && e.Resource != "image.generate") {
				return
			}
			m.RecordEvent("fairqueue", string(e.Name), e.Resource, e.Outcome, e.Duration)
			switch e.Name {
			case fairqueue.TelemetryRabbitDepth, fairqueue.TelemetryActiveTenants, fairqueue.TelemetryRing, fairqueue.TelemetryRingMembers, fairqueue.TelemetryGlobalInflight, fairqueue.TelemetryReservation, fairqueue.TelemetryProcessingTurn:
				// These signals also emit transitions/errors with zero Value. Only
				// successful samples are gauge values, never reset depth on error.
				if e.Outcome == "ok" {
					m.Samples.WithLabelValues(e.Resource, string(e.Name), e.ReservationKind).Set(float64(e.Value))
					m.SampleTime.WithLabelValues(e.Resource, string(e.Name), e.ReservationKind).Set(float64(time.Now().Unix()))
				}
			}
		}), event)
	})
}

// RegisterOperationalCollectors is called before HTTP admission. Collect only
// cached health and database/sql pool stats; scraping performs no dependency I/O.
func (g *Gateway) RegisterOperationalCollectors(m *observability.Metrics) {
	if m == nil {
		return
	}
	if db, ok := g.store.(interface{ DB() *sql.DB }); ok {
		m.Registry.MustRegister(collectors.NewDBStatsCollector(db.DB(), "primary"))
	}
	m.Registry.MustRegister(&queueHealthCollector{gateway: g,
		enabled:    prometheus.NewDesc("bkcrab_fairqueue_enabled", "Whether the resource is enabled in this process.", []string{"resource"}, nil),
		health:     prometheus.NewDesc("bkcrab_fairqueue_healthy", "Cached overall queue health (1 healthy, 0 otherwise).", []string{"resource"}, nil),
		dependency: prometheus.NewDesc("bkcrab_fairqueue_dependency_up", "Cached dependency health (1 ok, 0 otherwise).", []string{"resource", "dependency"}, nil),
		gate:       prometheus.NewDesc("bkcrab_fairqueue_gate_open", "Cached scheduler admission gate.", []string{"resource"}, nil),
		lag:        prometheus.NewDesc("bkcrab_fairqueue_loop_lag_seconds", "Time since the last successful loop; absent before first success.", []string{"resource", "loop"}, nil),
	})
}

type queueHealthCollector struct {
	gateway                                *Gateway
	enabled, health, dependency, gate, lag *prometheus.Desc
}

func (c *queueHealthCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.enabled, c.health, c.dependency, c.gate, c.lag} {
		ch <- d
	}
}
func (c *queueHealthCollector) Collect(ch chan<- prometheus.Metric) {
	h := c.gateway.FairQueueHealthSnapshot().FairQueue
	primary := "rag.index"
	if c.gateway.ragFairQueue == nil && c.gateway.imageFairQueue != nil {
		primary = "image.generate"
	}
	c.collectResource(ch, primary, h)
	if extra, ok := h.Resources["image.generate"]; ok && primary != "image.generate" {
		c.collectResource(ch, "image.generate", extra)
	}
}
func (c *queueHealthCollector) collectResource(ch chan<- prometheus.Metric, resource string, h fairqueue.FairQueueHealthSnapshot) {
	flag := func(v bool) float64 {
		if v {
			return 1
		}
		return 0
	}
	ch <- prometheus.MustNewConstMetric(c.enabled, prometheus.GaugeValue, flag(h.Enabled), resource)
	if !h.Enabled {
		return
	}
	ch <- prometheus.MustNewConstMetric(c.health, prometheus.GaugeValue, flag(h.Status == "healthy"), resource)
	ch <- prometheus.MustNewConstMetric(c.gate, prometheus.GaugeValue, flag(h.GateOpen), resource)
	for dependency, state := range map[string]string{"mysql": h.MySQL.Status, "redis": h.Redis.Status, "rabbitmq": h.Rabbit.Status} {
		ch <- prometheus.MustNewConstMetric(c.dependency, prometheus.GaugeValue, flag(state == "ok"), resource, dependency)
	}
	for loop, at := range map[string]*time.Time{"scheduler": h.Loops.Scheduler.LastSuccessAt, "dispatcher": h.Loops.Dispatcher.LastSuccessAt, "sweeper": h.Loops.Sweeper.LastSuccessAt, "reconciler": h.Loops.Reconciler.LastSuccessAt} {
		if at != nil {
			ch <- prometheus.MustNewConstMetric(c.lag, prometheus.GaugeValue, max(0, time.Since(*at).Seconds()), resource, loop)
		}
	}
}
