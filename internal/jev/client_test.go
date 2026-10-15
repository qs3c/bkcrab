package jev

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, h http.HandlerFunc, retries int) *Client {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	c, e := New(Options{Endpoint: s.URL, APIKey: "secret", Model: "jev-1.13.0", Timeout: time.Second, Concurrency: 1, Retries: retries, MaxCostUSD: 1, InputCostPerMillion: .042})
	if e != nil {
		t.Fatal(e)
	}
	return c
}

var question = map[string]Question{"q": Noul("Is this supported?", "supported", "not supported")}

const valid = `{"model":"jev-1.13.0","answers":{"q":{"type":"noul","noul":0.8}},"usage":{"input_tokens":10,"output_tokens":1}}`

func TestDecideContract(t *testing.T) {
	for _, body := range []string{`{}`, strings.Replace(valid, "jev-1.13.0", "jev-other", 1), strings.Replace(valid, `"q":`, `"other":`, 1), strings.Replace(valid, `0.8`, `2`, 1), strings.Replace(valid, `0.8`, `null`, 1), strings.Replace(valid, `"noul","noul"`, `"score","noul"`, 1), valid + `{}`, strings.Replace(valid, `"input_tokens":10`, `"input_tokens":-1`, 1), strings.Replace(valid, `"output_tokens":1`, `"cost":-1,"output_tokens":1`, 1)} {
		t.Run(body, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }, 0)
			if _, e := c.Decide(context.Background(), "data", question); e == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}
func TestRetryUsageAndNoLifetimeBudget(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		if calls == 1 {
			w.WriteHeader(503)
			return
		}
		fmt.Fprint(w, valid)
	}, 1)
	a, e := c.Decide(context.Background(), "data", question)
	if e != nil || a.Probabilities["q"] != .8 || a.Usage.Attempts != 2 || !a.Usage.Unknown || a.Usage.EstimatedCostUSD <= 0 {
		t.Fatalf("unexpected result %+v %v", a, e)
	}
	for i := 0; i < 5; i++ {
		if _, e = c.Decide(context.Background(), "data", question); e != nil {
			t.Fatal(e)
		}
	}
}
func TestPermanentErrorNoRetry(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(401); fmt.Fprint(w, "secret") }, 2)
	_, e := c.Decide(context.Background(), "data", question)
	if e == nil || calls != 1 || strings.Contains(e.Error(), "secret") {
		t.Fatal("permanent error leaked or retried", e, calls)
	}
}
func TestBoundsCancellationAndBudget(t *testing.T) {
	calls := 0
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls++; fmt.Fprint(w, valid) }, 0)
	if _, e := c.Decide(context.Background(), strings.Repeat("x", MaxRequestBytes), question); e == nil {
		t.Fatal("accepted oversized")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := c.Decide(ctx, "data", question); e == nil {
		t.Fatal("accepted canceled")
	}
	c.opts.MaxCostUSD = 1e-12
	if _, e := c.Decide(context.Background(), "data", question); e == nil {
		t.Fatal("budget ignored")
	}
	if calls != 0 {
		t.Fatal("unexpected network calls")
	}
}
func TestRetryAfterHonorsDeadline(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Retry-After", "60"); w.WriteHeader(429) }, 2)
	start := time.Now()
	_, e := c.Decide(context.Background(), "data", question)
	if e == nil || time.Since(start) > time.Second {
		t.Fatal("unbounded retry")
	}
}

func TestSharedConcurrencyAndCanceledQueue(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release
		fmt.Fprint(w, valid)
	}, 0)
	firstDone := make(chan error, 1)
	go func() { _, err := c.Decide(context.Background(), "first", question); firstDone <- err }()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := c.Decide(ctx, "second", question)
	close(release)
	if firstErr := <-firstDone; firstErr != nil {
		t.Fatal(firstErr)
	}
	if err == nil || result.Usage.Attempts != 0 || calls.Load() != 1 {
		t.Fatalf("queued cancellation dispatched or consumed usage: %+v %v calls=%d", result, err, calls.Load())
	}
}
