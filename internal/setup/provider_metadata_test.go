package setup

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
)

type providerTestTransport func(*http.Request) (*http.Response, error)

func (fn providerTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func TestConnectionTestSendsOpenCodeSession(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	var ids []string
	http.DefaultTransport = providerTestTransport(func(r *http.Request) (*http.Response, error) {
		ids = append(ids, r.Header.Get("x-opencode-session"))
		if ids[len(ids)-1] == "" || !strings.HasPrefix(r.UserAgent(), "BkCrab/") || r.Header.Get("Authorization") != "Bearer key" {
			t.Fatal("connection test omitted metadata or changed authentication")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))}, nil
	})
	ctx := provider.WithSession(context.Background(), "connection-test", "one")
	input := testProviderRequest{APIBase: "https://opencode.ai/zen/go/v1", APIKey: "key", Model: "deepseek-v4-flash"}
	for _, callCtx := range []context.Context{ctx, ctx, context.Background(), context.Background()} {
		if result := runProviderTest(callCtx, input); result["ok"] != true {
			t.Fatalf("connection failed: %+v", result)
		}
	}
	if ids[0] != ids[1] || ids[2] == ids[3] || ids[0] == ids[2] {
		t.Fatalf("connection session scope mismatch: %v", ids)
	}
}
