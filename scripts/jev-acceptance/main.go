// jev-acceptance runs explicit, bounded live checks against production adapters.
// Credentials come from the environment; inputs must be public or synthetic.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/jev"
	"github.com/qs3c/bkcrab/internal/jeveval"
	"github.com/qs3c/bkcrab/internal/memory"
	"github.com/qs3c/bkcrab/internal/rag/rerank"
)

type sample struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Language  string   `json:"language"`
	Expected  string   `json:"expected"`
	Entries   []string `json:"entries"`
	Query     string   `json:"query"`
	Reference string   `json:"reference"`
	Contexts  []string `json:"contexts"`
	Claims    []string `json:"claims"`
	Relevant  []int    `json:"relevant"`
}
type metered struct {
	client jev.Decider
	budget float64
	used   float64
	calls  []jev.Result
}

func (m *metered) Decide(ctx context.Context, state any, questions map[string]jev.Question) (jev.Result, error) {
	// Every request can consume up to .01 USD, including the configured retry.
	if m.used+.01 > m.budget+1e-9 {
		return jev.Result{}, errors.New("acceptance budget exhausted")
	}
	r, e := m.client.Decide(ctx, state, questions)
	m.used += r.Usage.EstimatedCostUSD
	m.calls = append(m.calls, r)
	return r, e
}
func main() {
	input := flag.String("input", "", "public or synthetic JSON sample array")
	budget := flag.Float64("budget-usd", .25, "estimated cumulative Jev cost ceiling")
	maxCases := flag.Int("limit", 100, "maximum number of cases")
	timeout := flag.Duration("timeout", 5*time.Second, "per-Decision deadline including retries")
	qwenEndpoint := flag.String("qwen-endpoint", "", "optional local Qwen baseline for rerank cases")
	proxy := flag.Bool("proxy", false, "serve an ephemeral loopback-only fixed-rubric bridge for the Ragas smoke")
	flag.Parse()
	if *budget <= 0 || *budget > .5 || *maxCases < 1 || *maxCases > 200 {
		panic("invalid acceptance limits")
	}
	client, err := jev.New(jev.Options{Endpoint: os.Getenv("BKCRAB_JEV_ENDPOINT"), APIKey: os.Getenv("BKCRAB_JEV_API_KEY"), Model: os.Getenv("BKCRAB_JEV_MODEL"), Timeout: *timeout, Concurrency: 1, Retries: 1, MaxCostUSD: .01, InputCostPerMillion: .042})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if *proxy {
		serveProxy(&metered{client: client, budget: *budget}, client.Model())
		return
	}
	data, err := os.ReadFile(*input)
	if err != nil {
		panic("input unavailable")
	}
	var samples []sample
	if json.Unmarshal(data, &samples) != nil || len(samples) == 0 {
		panic("invalid input")
	}
	meter := &metered{client: client, budget: *budget}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	for i, s := range samples {
		if i >= *maxCases {
			break
		}
		before := len(meter.calls)
		start := time.Now()
		row := map[string]any{"id": s.ID, "kind": s.Kind, "language": s.Language, "expected": s.Expected, "startedAt": start.UTC(), "status": "ok"}
		var e error
		switch s.Kind {
		case "guard":
			r := memory.NewJevGuard(meter, "shadow").Review(ctx, memory.TargetMemory, s.Entries)
			row["action"] = r.Action
			if r.Action == "unavailable" {
				e = errors.New("guard unavailable")
			}
		case "rerank":
			var r rerank.Reranker
			if *qwenEndpoint != "" {
				r, e = rerank.NewQwen3HTTP(*qwenEndpoint, "", 180*time.Second, 1)
				row["arm"] = "qwen"
			} else {
				r, e = rerank.NewProductionJev(meter, client.Model(), 20, 60*time.Second)
				row["arm"] = "jev"
			}
			var ranked []rerank.Result
			if e == nil {
				ranked, e = r.Rerank(ctx, s.Query, s.Contexts, len(s.Contexts))
			}
			row["ranks"] = ranked
			row["relevant"] = s.Relevant
		case "precision", "support":
			r := (&jeveval.Service{Client: meter, Model: client.Model()}).Evaluate(ctx, jeveval.Request{Kind: s.Kind, Model: client.Model(), Query: s.Query, Reference: s.Reference, Contexts: s.Contexts, Claims: s.Claims})
			row["verdict"] = r
			if r.Error != "" {
				e = errors.New(r.Error)
			}
		default:
			e = errors.New("unknown sample kind")
		}
		row["durationMs"] = time.Since(start).Milliseconds()
		row["calls"] = meter.calls[before:]
		row["cumulativeEstimatedCostUsd"] = meter.used
		if e != nil {
			row["status"] = "error"
			row["error"] = e.Error()
		}
		if err := json.NewEncoder(os.Stdout).Encode(row); err != nil {
			os.Exit(1)
		}
		if ctx.Err() != nil || meter.used+.01 > *budget {
			break
		}
	}
}

// This isolated bridge exercises the production rubric/client, not gateway
// authorization or deployment routing. It cannot modify the application store.
func serveProxy(meter *metered, model string) {
	key := os.Getenv("JEV_ACCEPTANCE_INTERNAL_KEY")
	if len(key) < 24 {
		panic("missing local bridge credential")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic("cannot bind loopback bridge")
	}
	var mu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/jev" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+key)) != 1 || r.Header.Get("X-BkCrab-Eval-Owner") != "acceptance" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		var request jeveval.Request
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10))
		d.DisallowUnknownFields()
		if d.Decode(&request) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		start := time.Now()
		response := (&jeveval.Service{Client: meter, Model: model}).Evaluate(r.Context(), request)
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"kind": "proxy", "durationMs": time.Since(start).Milliseconds(), "result": response, "cumulativeEstimatedCostUsd": meter.used})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"endpoint": "http://" + listener.Addr().String() + "/jev"})
	time.AfterFunc(10*time.Minute, func() { _ = server.Close() })
	_ = server.Serve(listener)
}
