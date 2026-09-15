// Package observability owns process-local operational metrics. Labels must be
// bounded categories, never identities, payloads, URLs or error messages.
package observability

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

var current atomic.Pointer[Metrics]

// SetDefault is called at process startup, before workers and handlers start.
// A nil registry disables instrumentation. Tests can use isolated registries.
func SetDefault(m *Metrics) { current.Store(m) }
func Current() *Metrics     { return current.Load() }

type Metrics struct {
	QueueWait     *prometheus.HistogramVec
	Registry      *prometheus.Registry
	Requests      *prometheus.CounterVec
	Latency       *prometheus.HistogramVec
	Inflight      *prometheus.GaugeVec
	Tokens        *prometheus.CounterVec
	FirstText     *prometheus.HistogramVec
	Events        *prometheus.CounterVec
	EventDuration *prometheus.HistogramVec
	Samples       *prometheus.GaugeVec
	SampleTime    *prometheus.GaugeVec
	httpRequests  *prometheus.CounterVec
	httpDuration  *prometheus.HistogramVec
	httpInflight  *prometheus.GaugeVec
}

func New() *Metrics {
	buckets := []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800}
	m := &Metrics{Registry: prometheus.NewRegistry()}
	m.QueueWait = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "bkcrab_task_queue_wait_seconds", Help: "Creation to first execution for tasks with no retry; retries excluded.", Buckets: buckets}, []string{"resource"})
	m.Requests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bkcrab_operations_total", Help: "Completed operations at the instrumented boundary."}, []string{"component", "operation", "mode", "outcome"})
	m.Latency = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "bkcrab_operation_duration_seconds", Help: "Operation duration, including complete streamed model responses.", Buckets: buckets}, []string{"component", "operation", "mode"})
	m.Inflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bkcrab_operations_inflight", Help: "Currently executing operations in this process."}, []string{"component", "operation", "mode"})
	m.Tokens = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bkcrab_llm_tokens_total", Help: "Provider-reported tokens; missing usage is not estimated. Cache categories follow provider semantics."}, []string{"protocol", "kind"})
	m.FirstText = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "bkcrab_llm_first_text_seconds", Help: "Time to first nonempty text delta, excluding reasoning/tool-only streams.", Buckets: buckets}, []string{"protocol"})
	m.Events = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bkcrab_telemetry_events_total", Help: "Telemetry event occurrences, not task or queue depth totals."}, []string{"component", "signal", "operation", "outcome"})
	m.EventDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "bkcrab_telemetry_duration_seconds", Help: "Positive durations supplied by domain telemetry.", Buckets: buckets}, []string{"component", "signal", "operation"})
	m.Samples = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bkcrab_fairqueue_sample", Help: "Last successful shared queue sample. Use max, not sum, across replicas; check sample timestamp."}, []string{"resource", "signal", "kind"})
	m.SampleTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bkcrab_fairqueue_sample_timestamp_seconds", Help: "Unix timestamp of last successful queue sample."}, []string{"resource", "signal", "kind"})
	m.httpRequests = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "bkcrab_http_requests_total", Help: "Completed HTTP requests or upgraded connections."}, []string{"method", "route", "status", "mode"})
	m.httpDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "bkcrab_http_request_duration_seconds", Help: "Handler lifetime. Stream/websocket lifetimes are not ordinary API latency.", Buckets: buckets}, []string{"method", "route", "mode"})
	m.httpInflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bkcrab_http_requests_inflight", Help: "Active HTTP handlers, including streams and upgrades."}, []string{"method", "route", "mode"})
	m.Registry.MustRegister(m.QueueWait, collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), m.Requests, m.Latency, m.Inflight, m.Tokens, m.FirstText, m.Events, m.EventDuration, m.Samples, m.SampleTime, m.httpRequests, m.httpDuration, m.httpInflight)
	return m
}

func Outcome(err error) string {
	if err == nil {
		return "ok"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var n net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &n) && n.Timeout() {
		return "timeout"
	}
	return "error"
}

// Begin returns a completion callback for a single operation. Call exactly once.
// All three dimensions are call-site constants or closed classifications.
func (m *Metrics) Begin(component, operation, mode string) func(error) {
	if m == nil {
		return func(error) {}
	}
	started := time.Now()
	g := m.Inflight.WithLabelValues(component, operation, mode)
	g.Inc()
	return func(err error) {
		g.Dec()
		m.Requests.WithLabelValues(component, operation, mode, Outcome(err)).Inc()
		m.Latency.WithLabelValues(component, operation, mode).Observe(time.Since(started).Seconds())
	}
}

func (m *Metrics) RecordTokens(protocol string, input, output, cacheRead, cacheWrite int) {
	if m == nil {
		return
	}
	for kind, n := range map[string]int{"input": input, "output": output, "cache_read": cacheRead, "cache_write": cacheWrite} {
		if n > 0 {
			m.Tokens.WithLabelValues(protocol, kind).Add(float64(n))
		}
	}
}

func (m *Metrics) RecordEvent(component, signal, operation, outcome string, duration time.Duration) {
	if m == nil {
		return
	}
	m.Events.WithLabelValues(component, signal, operation, outcome).Inc()
	if duration > 0 {
		m.EventDuration.WithLabelValues(component, signal, operation).Observe(duration.Seconds())
	}
}

// ToolOperation deliberately collapses user-defined MCP/plugin tool names.
func ToolOperation(name string) string {
	switch name {
	case "exec", "read_file", "write_file", "edit_file", "apply_patch", "list_dir", "web_search", "web_fetch", "rag_search", "image_generate", "load_skill", "recall_tool_result":
		return name
	default:
		return "other"
	}
}

// RecordQueueWait measures initial admission delay. Retried task age includes
// previous execution/backoff, so it must not be mixed into queue wait.
func (m *Metrics) RecordQueueWait(resource string, created time.Time, retries int) {
	if m != nil && retries == 0 && !created.IsZero() {
		m.QueueWait.WithLabelValues(resource).Observe(max(0, time.Since(created).Seconds()))
	}
}
