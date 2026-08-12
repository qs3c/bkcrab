package imagegen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	domain "github.com/qs3c/bkcrab/internal/imagegen"
)

func TestReplicateGenerateMapsTypedRequestAndOutput(t *testing.T) {
	var received struct {
		Input map[string]any `json:"input"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Prefer") != "wait" || r.Header.Get("Authorization") != "Bearer replicate-key" {
			t.Errorf("headers: %#v", r.Header)
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "succeeded", "output": []string{"https://images.example/one.webp", "https://images.example/two.webp"}})
	}))
	defer server.Close()
	result, err := (&Replicate{}).Generate(context.Background(), domain.ResolvedProviderConfig{APIKey: "replicate-key", Endpoint: server.URL}, domain.GenerateRequest{Prompt: "draw", Size: domain.SizeLandscape, Count: 2})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if received.Input["aspect_ratio"] != "4:3" || received.Input["num_outputs"] != float64(2) || len(result.Images) != 2 {
		t.Fatalf("mapping/result: request=%#v result=%#v", received, result)
	}
}

func TestReplicateGenerateClassifiesStatusAndMalformedOutput(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		kind   domain.ErrorKind
	}{
		{name: "processing", status: 201, body: `{"status":"processing"}`, kind: domain.ErrorUpstreamTransient},
		{name: "failed safety", status: 201, body: `{"status":"failed","error":"content policy violation"}`, kind: domain.ErrorSafetyRejected},
		{name: "malformed output", status: 201, body: `{"status":"succeeded","output":{"bad":true}}`, kind: domain.ErrorMalformedResult},
		{name: "rate", status: 429, body: `{}`, kind: domain.ErrorRateLimited},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			_, err := (&Replicate{}).Generate(context.Background(), domain.ResolvedProviderConfig{APIKey: "key", Endpoint: server.URL}, domain.GenerateRequest{Prompt: "draw", Size: domain.SizeSquare, Count: 1})
			if got := domain.ProviderErrorKind(err); got != tt.kind {
				t.Fatalf("kind=%s want=%s err=%v", got, tt.kind, err)
			}
		})
	}
}
