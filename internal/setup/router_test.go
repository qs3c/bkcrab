package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestWrapBridgesPathValues is the linchpin of the gin migration: handler
// bodies still call r.PathValue(...), so wrap() must copy gin's path
// params onto the request. It also verifies that catch-all (*path)
// values get their leading slash trimmed to match net/http's {path...}
// semantics, and that values survive a middleware that swaps the request
// context (as the auth middleware does via req.WithContext).
func TestWrapBridgesPathValues(t *testing.T) {
	gin.SetMode(gin.TestMode)

	type ctxKey string
	// stand-in for the auth middleware: rebinds the request context, then
	// calls the next handler. Path values set before this must still read.
	withCtx := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			next(w, r.WithContext(context.WithValue(r.Context(), ctxKey("k"), "v")))
		}
	}

	r := gin.New()
	r.GET("/api/agents/:id/files/*path", wrap(withCtx(func(w http.ResponseWriter, req *http.Request) {
		if got := req.Context().Value(ctxKey("k")); got != "v" {
			t.Errorf("context not propagated: got %v", got)
		}
		w.Header().Set("X-Id", req.PathValue("id"))
		w.Header().Set("X-Path", req.PathValue("path"))
		w.WriteHeader(http.StatusOK)
	})))

	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/agents/abc/files/sub/dir/x.txt", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := rr.Header().Get("X-Id"); got != "abc" {
		t.Errorf("id = %q, want abc", got)
	}
	// {path...} under ServeMux yielded "sub/dir/x.txt" (no leading slash).
	if got := rr.Header().Get("X-Path"); got != "sub/dir/x.txt" {
		t.Errorf("path = %q, want sub/dir/x.txt", got)
	}
}
