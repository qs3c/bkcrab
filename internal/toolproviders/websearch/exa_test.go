package websearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExaSearchRequestsAndReturnsHighlights(t *testing.T) {
	previous := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = previous })
	for _, explicitCount := range []bool{false, true} {
		http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://api.exa.ai/search" || r.Method != http.MethodPost || r.Header.Get("x-api-key") != "fixture-key" {
				t.Fatal("wrong Exa request")
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["query"] != "Python documentation" || body["type"] != "auto" || body["contents"].(map[string]any)["highlights"] != true {
				t.Fatalf("incorrect request: %#v", body)
			}
			_, hasCount := body["numResults"]
			if hasCount != explicitCount || (hasCount && body["numResults"] != float64(3)) {
				t.Fatalf("unexpected result count: %#v", body)
			}
			wantFields := 3
			if explicitCount {
				wantFields++
			}
			if len(body) != wantFields {
				t.Fatalf("unrequested parameters: %#v", body)
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"results":[{"title":"Python","url":"https://docs.python.org/","highlights":["第一段摘录","Second excerpt"]}]}`)), Header: make(http.Header)}, nil
		})}
		args := map[string]any{"query": "Python documentation"}
		if explicitCount {
			args["count"] = 3
		}
		out, err := (&Exa{}).Execute(context.Background(), toolproviders.Request{Args: args, Config: toolproviders.ProviderConfig{APIKey: "fixture-key"}})
		if err != nil || !strings.Contains(out.Text, "第一段摘录") || !strings.Contains(out.Text, "Second excerpt") || !strings.Contains(out.Text, "https://docs.python.org/") {
			t.Fatalf("missing search content: %q %v", out.Text, err)
		}
	}
}
