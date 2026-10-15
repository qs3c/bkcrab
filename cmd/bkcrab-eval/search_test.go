package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/agent/tools"
)

func TestSearchToolUsesProductionProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("q") != "python documentation" || r.URL.Query().Get("format") != "json" {
			t.Errorf("unexpected search request: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"Python docs","url":"https://docs.python.org/3/","content":"Official documentation"}]}`))
	}))
	defer server.Close()
	chain, err := searchChain("searxng", server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	r := tools.NewRegistry(t.TempDir(), t.TempDir())
	tools.RegisterWebSearchChain(r, chain)
	out, err := r.Execute(context.Background(), "web_search", `{"query":"python documentation"}`)
	if err != nil || !strings.Contains(out, "https://docs.python.org/3/") {
		t.Fatalf("search result: %q %v", out, err)
	}
}

func TestSearchConfigurationRejectsUnusableTools(t *testing.T) {
	for _, c := range [][3]string{{"exa", "", ""}, {"brave", "", ""}, {"searxng", "", ""}, {"searxng", "http://user:pass@host", ""}, {"searxng", "https://host?key=secret", ""}, {"unknown", "", "key"}} {
		if _, err := searchChain(c[0], c[1], c[2]); err == nil {
			t.Fatalf("accepted invalid configuration for %s", c[0])
		}
	}
	if chain, err := searchChain("", "", ""); err != nil || chain != nil {
		t.Fatalf("disabled search: %v %v", chain, err)
	}
	for _, name := range []string{"exa", "brave"} {
		if chain, err := searchChain(name, "", "fixture-key"); err != nil || !chain.Available() {
			t.Fatalf("configured %s: %v", name, err)
		}
	}
}

func TestExaRelayUsesLocalEndpointWithoutRealKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/exa" || r.Header.Get("x-api-key") != "relay-only" {
			t.Errorf("unexpected relay request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"results":[{"title":"Python","url":"https://docs.python.org","highlights":["Official docs"]}]}`))
	}))
	defer server.Close()
	chain, err := searchChain("exa", server.URL+"/search/exa", "")
	if err != nil {
		t.Fatal(err)
	}
	r := tools.NewRegistry(t.TempDir(), t.TempDir())
	tools.RegisterWebSearchChain(r, chain)
	out, err := r.Execute(context.Background(), "web_search", `{"query":"python documentation"}`)
	if err != nil || !strings.Contains(out, "Official docs") {
		t.Fatalf("relay result: %s %v", out, err)
	}
	for _, endpoint := range []string{"https://api.exa.ai/search", "http://example.com/search/exa", server.URL + "/search/exa?key=x"} {
		if _, err := searchChain("exa", endpoint, ""); err == nil {
			t.Fatal("accepted non-local relay")
		}
	}
	if _, err := searchChain("exa", server.URL+"/search/exa", "real-secret"); err == nil {
		t.Fatal("accepted real key for relay")
	}
}
