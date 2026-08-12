package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/qs3c/bkcrab/internal/imagegen"
	"github.com/qs3c/bkcrab/internal/toolproviders"
)

func TestTypedBackendsDeclareCapabilities(t *testing.T) {
	backends := []domain.Backend{&OpenAI{}, &Fal{}, &Replicate{}}
	for _, backend := range backends {
		capability := backend.Capability("")
		if backend.Name() == "" || capability.MaxImagesPerCall < 1 || len(capability.SupportedSizes) != 3 {
			t.Fatalf("invalid capability for %T: %#v", backend, capability)
		}
	}
	if got := (&OpenAI{}).Capability("dall-e-3").MaxImagesPerCall; got != 1 {
		t.Fatalf("dall-e-3 max count=%d", got)
	}
}

func TestLegacyOpenAIExecuteUsesTypedBackendAndPreservesClamp(t *testing.T) {
	requestedN := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			N int `json:"n"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		requestedN = body.N
		item := map[string]string{"b64_json": base64.StdEncoding.EncodeToString(tinyPNG)}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{item, item, item, item}})
	}))
	defer server.Close()

	response, err := (&OpenAI{}).Execute(context.Background(), toolproviders.Request{
		Args:   map[string]any{"prompt": "legacy prompt", "size": "1024x1024", "n": float64(9)},
		Config: toolproviders.ProviderConfig{APIKey: "key", Endpoint: server.URL},
	})
	if err != nil {
		t.Fatalf("legacy execute: %v", err)
	}
	if requestedN != 4 {
		t.Fatalf("legacy n>4 clamp changed: got %d", requestedN)
	}
	if strings.Count(response.Text, "![image ") != 4 || !strings.Contains(response.Text, "data:image/png;base64,") {
		t.Fatalf("legacy markdown changed: %s", response.Text)
	}
	if _, ok := response.Raw.(domain.ProviderResult); !ok {
		t.Fatalf("legacy response should retain typed raw result: %T", response.Raw)
	}
}
