// A synthetic metrics fixture for the isolated monitoring smoke test.
// It never opens application storage or calls an external model.
package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/qs3c/bkcrab/internal/observability"
)

func main() {
	m := observability.New()
	// Unopened SQL pool: exports real database/sql stats without a database.
	m.Registry.MustRegister(collectors.NewDBStatsCollector(new(sql.DB), "primary"))
	for _, name := range []string{"bkcrab_fairqueue_enabled", "bkcrab_fairqueue_healthy", "bkcrab_fairqueue_gate_open"} {
		g := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: "Synthetic smoke fixture.", ConstLabels: prometheus.Labels{"resource": "rag.index"}})
		g.Set(1)
		m.Registry.MustRegister(g)
	}
	dep := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bkcrab_fairqueue_dependency_up", Help: "Synthetic smoke fixture."}, []string{"resource", "dependency"})
	for _, d := range []string{"mysql", "redis", "rabbitmq"} {
		dep.WithLabelValues("rag.index", d).Set(1)
	}
	m.Registry.MustRegister(dep)
	lag := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "bkcrab_fairqueue_loop_lag_seconds", Help: "Synthetic smoke fixture."}, []string{"resource", "loop"})
	lag.WithLabelValues("rag.index", "scheduler").Set(1)
	m.Registry.MustRegister(lag)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/smoke/{id}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("error") == "1" {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(200)
		}
	})
	handler := m.Middleware(mux)
	server, err := m.StartServer(":18954")
	if err != nil {
		panic(err)
	}
	defer server.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, url := range []string{"/api/smoke/one", "/api/smoke/two?error=1"} {
				handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", url, nil))
			}
			for _, op := range []struct{ component, operation, mode string }{{"llm", "openai", "stream"}, {"tool", "read_file", "call"}, {"rag", "search", "call"}, {"rag", "index", "call"}} {
				m.Begin(op.component, op.operation, op.mode)(nil)
				m.Begin(op.component, op.operation, op.mode)(errors.New("synthetic error"))
			}
			m.RecordTokens("openai", 7, 3, 0, 0)
			m.FirstText.WithLabelValues("openai").Observe(.1)
			m.RecordEvent("rag", "rag.parser.document", "", "ok", 100*time.Millisecond)
			m.RecordEvent("rag", "rag.result_cache", "parse_artifact", "hit", 0)
			m.RecordEvent("fairqueue", "task.run", "rag.index", "completed", time.Second)
			m.RecordEvent("fairqueue", "recovery.run", "rag.index", "error", time.Second)
			for _, signal := range []string{"rabbit.ready_depth", "redis.inflight"} {
				m.Samples.WithLabelValues("rag.index", signal, "").Set(2)
				m.SampleTime.WithLabelValues("rag.index", signal, "").Set(float64(time.Now().Unix()))
			}
			m.RecordQueueWait("rag.index", time.Now().Add(-time.Second), 0)
		}
	}
}
