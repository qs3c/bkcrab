package observability

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func exposition(m *Metrics) string {
	w := httptest.NewRecorder()
	promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}).ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	return w.Body.String()
}

func TestOperationsAndBoundedToolLabels(t *testing.T) {
	m := New()
	finish := m.Begin("tool", ToolOperation("mcp_private_user_9381"), "call")
	if n := testutil.ToFloat64(m.Inflight.WithLabelValues("tool", "other", "call")); n != 1 {
		t.Fatal(n)
	}
	finish(context.Canceled)
	if n := testutil.ToFloat64(m.Inflight.WithLabelValues("tool", "other", "call")); n != 0 {
		t.Fatal(n)
	}
	if n := testutil.ToFloat64(m.Requests.WithLabelValues("tool", "other", "call", "canceled")); n != 1 {
		t.Fatal(n)
	}
	if strings.Contains(exposition(m), "private_user") {
		t.Fatal("identity leaked")
	}
	if Outcome(context.DeadlineExceeded) != "timeout" || Outcome(errors.New("secret")) != "error" {
		t.Fatal("outcome")
	}
	var disabled *Metrics
	disabled.Begin("tool", "other", "call")(nil)
	disabled.RecordTokens("openai", 2, 3, 0, 0)
}

func TestHTTPRouteNormalizationAndFlush(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(103)
		w.WriteHeader(201)
		_, _ = io.WriteString(w, "data: hello\n\n")
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Error(err)
		}
	})
	h := m.Middleware(mux)
	for i := 0; i < 100; i++ {
		r := httptest.NewRequest("GET", fmt.Sprintf("/api/items/secret-%d?token=secret", i), nil)
		r.Header.Set("Accept", "text/event-stream")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if !w.Flushed {
			t.Fatal("flush lost")
		}
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", fmt.Sprintf("/missing-%d", i), nil))
	}
	if n := testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "GET /api/items/{id}", "201", "stream")); n != 100 {
		t.Fatal(n)
	}
	if n := testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "unmatched", "404", "request")); n != 100 {
		t.Fatal(n)
	}
	if strings.Contains(exposition(m), "secret") || strings.Contains(exposition(m), "missing-") {
		t.Fatal("raw path leaked")
	}
}

func TestHTTPWebSocketUpgrade(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.Close()
		_ = c.WriteMessage(websocket.TextMessage, []byte("hello"))
	})
	s := httptest.NewServer(m.Middleware(mux))
	defer s.Close()
	c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(s.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, data, err := c.ReadMessage()
	if err != nil || string(data) != "hello" {
		t.Fatal(string(data), err)
	}
}

func TestHTTPPanicRecordedAndPropagated(t *testing.T) {
	m := New()
	mux := http.NewServeMux()
	mux.HandleFunc("/panic", func(http.ResponseWriter, *http.Request) { panic("test") })
	func() {
		defer func() {
			if recover() != "test" {
				t.Error("panic swallowed")
			}
		}()
		m.Middleware(mux).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/panic", nil))
	}()
	if n := testutil.ToFloat64(m.httpRequests.WithLabelValues("GET", "/panic", "500", "request")); n != 1 {
		t.Fatal(n)
	}
}

func TestMetricsServerLifecycle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if _, err := New().StartServer(addr); err == nil {
		t.Fatal("port conflict hidden")
	}
	ln.Close()
	s, err := New().StartServer(addr)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "go_goroutines") {
		t.Fatal("invalid scrape")
	}
	resp, err = http.Get("http://" + addr + "/private")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal("unexpected route")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	ln, err = net.Listen("tcp", addr)
	if err != nil {
		t.Fatal("listener leaked", err)
	}
	ln.Close()
}
