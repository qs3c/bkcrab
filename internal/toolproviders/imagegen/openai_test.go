package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	domain "github.com/qs3c/bkcrab/internal/imagegen"
)

var tinyPNG = []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0, 0, 0, 0}

func TestOpenAIGenerateMapsRequestAndDecodesTypedImages(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer current-key" {
			t.Errorf("authorization header = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}}})
	}))
	defer server.Close()

	result, err := (&OpenAI{}).Generate(context.Background(), domain.ResolvedProviderConfig{APIKey: "current-key", Endpoint: server.URL}, domain.GenerateRequest{Prompt: "draw", Size: domain.SizeLandscape, Count: 1})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if received["model"] != "gpt-image-1" || received["size"] != "1536x1024" || received["n"] != float64(1) {
		t.Fatalf("request mapping: %#v", received)
	}
	if len(result.Images) != 1 || string(result.Images[0].Bytes) != string(tinyPNG) || result.Images[0].MIMEType != "image/png" {
		t.Fatalf("typed image: %#v", result.Images)
	}
}

func TestOpenAIGenerateParsesURLAndClassifiesFailures(t *testing.T) {
	t.Run("url", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"url": "https://images.example/one.png"}}})
		}))
		defer server.Close()
		result, err := (&OpenAI{}).Generate(context.Background(), domain.ResolvedProviderConfig{APIKey: "key", Endpoint: server.URL}, domain.GenerateRequest{Prompt: "draw", Size: domain.SizeSquare, Count: 1})
		if err != nil || result.Images[0].SourceURL != "https://images.example/one.png" {
			t.Fatalf("URL result=%#v err=%v", result, err)
		}
	})

	tests := []struct {
		name   string
		status int
		body   string
		kind   domain.ErrorKind
	}{
		{name: "invalid", status: 400, body: `{"error":{"message":"bad size"}}`, kind: domain.ErrorInvalidRequest},
		{name: "safety", status: 400, body: `{"error":{"code":"content_policy_violation"}}`, kind: domain.ErrorSafetyRejected},
		{name: "auth", status: 401, body: `{}`, kind: domain.ErrorAuthConfig},
		{name: "rate", status: 429, body: `{}`, kind: domain.ErrorRateLimited},
		{name: "missing model", status: 404, body: `{}`, kind: domain.ErrorModelUnavailable},
		{name: "upstream", status: 503, body: `{}`, kind: domain.ErrorUpstreamTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			_, err := (&OpenAI{}).Generate(context.Background(), domain.ResolvedProviderConfig{APIKey: "key", Endpoint: server.URL}, domain.GenerateRequest{Prompt: "draw", Size: domain.SizeSquare, Count: 1})
			if got := domain.ProviderErrorKind(err); got != tt.kind {
				t.Fatalf("kind=%s want=%s err=%v", got, tt.kind, err)
			}
		})
	}
}

func TestOpenAIGenerateRejectsIncompleteAndMalformedResults(t *testing.T) {
	tests := []struct {
		name string
		body string
		kind domain.ErrorKind
	}{
		{name: "empty", body: `{"data":[]}`, kind: domain.ErrorEmptyResult},
		{name: "incomplete", body: `{"data":[{"url":"https://images.example/one.png"}]}`, kind: domain.ErrorIncompleteResult},
		{name: "bad json", body: `{`, kind: domain.ErrorMalformedResult},
		{name: "bad base64", body: `{"data":[{"b64_json":"%%%"}]}`, kind: domain.ErrorMalformedResult},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tt.body)) }))
			defer server.Close()
			count := 1
			if tt.name == "incomplete" {
				count = 2
			}
			_, err := (&OpenAI{}).Generate(context.Background(), domain.ResolvedProviderConfig{APIKey: "key", Endpoint: server.URL}, domain.GenerateRequest{Prompt: "draw", Size: domain.SizeSquare, Count: count})
			if got := domain.ProviderErrorKind(err); got != tt.kind {
				t.Fatalf("kind=%s want=%s err=%v", got, tt.kind, err)
			}
		})
	}
}
