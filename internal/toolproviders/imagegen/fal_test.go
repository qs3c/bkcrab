package imagegen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	domain "github.com/qs3c/bkcrab/internal/imagegen"
)

func TestFalGenerateMapsTypedRequestAndURLs(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Key fal-key" {
			t.Errorf("authorization = %q", got)
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		_ = json.NewEncoder(w).Encode(map[string]any{"images": []map[string]string{
			{"url": "https://images.example/one.png"}, {"url": "https://images.example/two.png"},
		}})
	}))
	defer server.Close()
	result, err := (&Fal{}).Generate(context.Background(), domain.ResolvedProviderConfig{APIKey: "fal-key", Endpoint: server.URL}, domain.GenerateRequest{Prompt: "draw", Size: domain.SizePortrait, Count: 2})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if received["image_size"] != "portrait_4_3" || received["num_images"] != float64(2) || len(result.Images) != 2 {
		t.Fatalf("mapping/result: request=%#v result=%#v", received, result)
	}
}

func TestFalGenerateClassifiesEmptyIncompleteAndCancellation(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		count int
		kind  domain.ErrorKind
	}{
		{name: "empty", body: `{"images":[]}`, count: 1, kind: domain.ErrorEmptyResult},
		{name: "incomplete", body: `{"images":[{"url":"https://images.example/one.png"}]}`, count: 2, kind: domain.ErrorIncompleteResult},
		{name: "malformed url", body: `{"images":[{"url":"javascript:bad"}]}`, count: 1, kind: domain.ErrorMalformedResult},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tt.body)) }))
			defer server.Close()
			_, err := (&Fal{}).Generate(context.Background(), domain.ResolvedProviderConfig{APIKey: "key", Endpoint: server.URL}, domain.GenerateRequest{Prompt: "draw", Size: domain.SizeSquare, Count: tt.count})
			if got := domain.ProviderErrorKind(err); got != tt.kind {
				t.Fatalf("kind=%s want=%s err=%v", got, tt.kind, err)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (&Fal{}).Generate(ctx, domain.ResolvedProviderConfig{APIKey: "key", Endpoint: "https://fal.invalid"}, domain.GenerateRequest{Prompt: "draw", Size: domain.SizeSquare, Count: 1})
	if got := domain.ProviderErrorKind(err); got != domain.ErrorUpstreamTransient {
		t.Fatalf("cancel kind=%s err=%v", got, err)
	}
}
