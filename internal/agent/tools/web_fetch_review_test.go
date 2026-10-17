package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetchReviewGates(t *testing.T) {
	for _, mode := range []string{"allow", "content", "redirect", "unavailable", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			pages := 0
			checks := []string{}
			reviewer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				checks = append(checks, body["stage"])
				if mode == "malformed" {
					_, _ = w.Write([]byte(`{"allowed":true}trailing-invalid-json`))
					return
				}
				if mode == "unavailable" {
					w.WriteHeader(503)
					return
				}
				if mode == body["stage"] || (mode == "redirect" && strings.HasSuffix(body["url"], "/answer")) {
					w.WriteHeader(403)
					return
				}
				if body["stage"] == "content" && !strings.Contains(body["content"], "PAGE_FIXTURE") {
					t.Error("missing visible content")
				}
				_, _ = w.Write([]byte(`{"allowed":true}`))
			}))
			defer reviewer.Close()
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pages++
				if mode == "redirect" && r.URL.Path != "/answer" {
					w.Header().Set("Location", "http://example.com/answer")
					w.WriteHeader(302)
					return
				}
				_, _ = w.Write([]byte("PAGE_FIXTURE"))
			}))
			defer proxy.Close()
			t.Setenv("EVAL_FETCH_REVIEW_URL", reviewer.URL)
			t.Setenv("HTTP_PROXY", proxy.URL)
			t.Setenv("NO_PROXY", "")
			t.Setenv("no_proxy", "")
			text, err := webFetchTool(context.Background(), nil, json.RawMessage(`{"url":"http://example.com/docs"}`))
			if mode == "allow" {
				if err != nil || text != "PAGE_FIXTURE" || len(checks) != 1 {
					t.Fatalf("allow: %q %v checks=%v", text, err, checks)
				}
			} else {
				if err == nil || text != "" || strings.Contains(err.Error(), "PAGE_FIXTURE") {
					t.Fatalf("page leaked or missing error: %q %v", text, err)
				}
				code := "EVAL_FETCH_DENIED"
				if mode == "unavailable" || mode == "malformed" {
					code = "EVAL_FETCH_REVIEW_UNAVAILABLE"
				}
				if !strings.Contains(err.Error(), code) {
					t.Fatalf("wrong error: %v", err)
				}
			}
			wantPages := 1
			if mode == "redirect" {
				wantPages = 2
			}
			if pages != wantPages {
				t.Fatalf("origin requests: got %d want %d", pages, wantPages)
			}
		})
	}
}
