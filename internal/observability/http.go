package observability

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Middleware resolves labels before dispatch so inflight metrics use the same
// bounded route as completions. Never use URL.Path or RequestURI as a label.
func (m *Metrics) Middleware(mux *http.ServeMux) http.Handler {
	if m == nil {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, route := mux.Handler(r)
		if route == "" {
			route = "unmatched"
		}
		if route == "/" {
			route = "static"
		}
		method := r.Method
		switch method {
		case "GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS", "CONNECT", "TRACE":
		default:
			method = "OTHER"
		}
		mode := "request"
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			mode = "websocket"
		} else if strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			mode = "stream"
		}
		// POST chat bodies can request streaming without an Accept header. These
		// routes are all kept out of ordinary HTTP latency SLOs without reading bodies.
		if mode == "request" && (strings.Contains(route, "/chat") || strings.Contains(route, "/events")) {
			mode = "chat"
		}
		started := time.Now()
		g := m.httpInflight.WithLabelValues(method, route, mode)
		g.Inc()
		rw := &responseWriter{ResponseWriter: w}
		defer func() {
			p := recover()
			status := rw.status
			if status == 0 {
				status = http.StatusOK
			}
			if p != nil {
				status = http.StatusInternalServerError
			}
			g.Dec()
			m.httpRequests.WithLabelValues(method, route, strconv.Itoa(status), mode).Inc()
			m.httpDuration.WithLabelValues(method, route, mode).Observe(time.Since(started).Seconds())
			if p != nil {
				panic(p)
			}
		}()
		mux.ServeHTTP(rw, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) WriteHeader(code int) {
	if w.status == 0 && (code >= 200 || code == 101) {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}
func (w *responseWriter) FlushError() error {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *responseWriter) Flush() { _ = w.FlushError() }
func (w *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, b, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil && w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return c, b, err
}
func (w *responseWriter) Push(target string, opts *http.PushOptions) error {
	if p, ok := w.ResponseWriter.(http.Pusher); ok {
		return p.Push(target, opts)
	}
	return http.ErrNotSupported
}
