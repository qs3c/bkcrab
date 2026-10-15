package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLLMHTTPRetry(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		status, failures, want int
	}{
		{"recovers502", 502, 1, 2}, {"bounded503", 503, 9, 3}, {"rejects400", 400, 9, 1}, {"rateLimit", 429, 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				if string(b) != `{"messages":[]}` {
					t.Errorf("body changed: %s", b)
				}
				if int(calls.Add(1)) <= tc.failures {
					w.WriteHeader(tc.status)
					return
				}
				fmt.Fprint(w, "ok")
			}))
			defer server.Close()
			req, _ := http.NewRequestWithContext(context.Background(), "POST", server.URL, strings.NewReader(`{"messages":[]}`))
			resp, err := doLLMRequest(server.Client(), req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if int(calls.Load()) != tc.want {
				t.Fatalf("calls=%d want=%d", calls.Load(), tc.want)
			}
		})
	}
}
func TestLLMRetryCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(502) }))
	defer server.Close()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL, strings.NewReader("{}"))
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := doLLMRequest(server.Client(), req)
	if err == nil || time.Since(start) > time.Second || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}
func TestLLMProviderRetryWiring(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if calls.Add(1) == 1 {
						w.WriteHeader(502)
						return
					}
					// A deterministic error proves both entrypoints made it past the first 502.
					w.WriteHeader(400)
				}))
				defer server.Close()
				var p Provider = NewOpenAI("test", server.URL)
				if protocol == "anthropic" {
					p = NewAnthropic("test", server.URL)
				}
				var err error
				if stream {
					_, err = p.ChatStream(context.Background(), nil, nil, "test", 10, 0)
				} else {
					_, err = p.Chat(context.Background(), nil, nil, "test", 10, 0)
				}
				if err == nil || calls.Load() != 2 {
					t.Fatalf("err=%v calls=%d", err, calls.Load())
				}
			})
		}
	}
}
func TestLLMDoesNotReplayStartedBody(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Length", "100")
		fmt.Fprint(w, "partial")
	}))
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL, strings.NewReader("{}"))
	resp, err := doLLMRequest(server.Client(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, err = io.ReadAll(resp.Body)
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestLLMTransportRecovery(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			c, _, e := w.(http.Hijacker).Hijack()
			if e != nil {
				t.Error(e)
				return
			}
			c.Close()
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL, strings.NewReader("{}"))
	resp, err := doLLMRequest(server.Client(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls.Load() != 2 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestLLMRetryAfterBound(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL, strings.NewReader("{}"))
	resp, err := doLLMRequest(server.Client(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls.Load() != 1 || resp.StatusCode != 429 {
		t.Fatalf("calls=%d status=%d", calls.Load(), resp.StatusCode)
	}
}
