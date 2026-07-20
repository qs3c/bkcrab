package vision

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/store"
)

var errTestCachePut = errors.New("test cache put failed")

type failingPutResultCache struct{}

func (failingPutResultCache) GetPage(context.Context, CacheScope, string) (PageTranscription, bool, error) {
	return PageTranscription{}, false, nil
}
func (failingPutResultCache) PutPage(context.Context, CacheScope, string, PageTranscription) error {
	return errTestCachePut
}
func (failingPutResultCache) GetImage(context.Context, CacheScope, string) (ImageDescription, bool, error) {
	return ImageDescription{}, false, nil
}
func (failingPutResultCache) PutImage(context.Context, CacheScope, string, ImageDescription) error {
	return errTestCachePut
}

func documentAIConfigForServer(t *testing.T, serverURL string) config.RAGDocumentAICfg {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	return config.RAGDocumentAICfg{
		APIType: "openai-compatible", Endpoint: serverURL + "/v1", APIKey: "test-secret",
		VisionModel: "vision-test", TimeoutMS: 5_000, VisionConcurrency: 1,
		VisionPromptVersion: "prompt-v1", AllowedEndpointHosts: []string{u.Hostname()},
		AllowPrivateEndpoint: true,
	}
}

func documentAILimits() config.RAGLimitsCfg {
	return config.RAGLimitsCfg{
		MaxAssetBytes: 1 << 20, MaxVisionInputBytes: 1 << 20, MaxImagePixels: 1_000_000,
		DisplayMaxEdge: 1024, MaxDocumentAIResponseBytes: 64 << 10,
		MaxDocumentAIOutputTokens: 512, MaxDocumentAIJSONDepth: 16,
	}
}

func writeOpenAIResponse(t *testing.T, w http.ResponseWriter, content string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 8},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenAIPageRepairBudgetAndCache(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-secret" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if request["response_format"] == nil || request["max_tokens"].(float64) != 512 {
			http.Error(w, "missing structured-output bounds", http.StatusBadRequest)
			return
		}
		messages := request["messages"].([]any)
		if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
			http.Error(w, "unsafe messages", http.StatusBadRequest)
			return
		}
		if call == 1 {
			writeOpenAIResponse(t, w, `{"markdown":"![x](rag-visual://missing)","visuals":[]}`)
			return
		}
		writeOpenAIResponse(t, w, `{"markdown":"## Page\n\n![x](rag-visual://v1)","visuals":[{"key":"v1","kind":"diagram","bbox":[0,0,1000,1000],"caption":"flow","ocrText":"A -> B","decorative":false,"confidence":0.9}]}`)
	}))
	defer server.Close()

	cache := NewMemoryCache(DefaultSchemaLimits())
	client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), cache)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	input, err := NormalizeImage(context.Background(), testPNG(t, 12, 12), "image/png", client.ImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	input.Scope = CacheScope{UserID: "u_1", KBID: "kb_1", DocID: "doc_1"}
	input.Format = "pdf"
	input.Location = document.SourceLocation{Kind: document.LocationPage, Index: 1, Label: "page 1"}
	ledger := &fakeBudgetLedger{}
	budget := newFakeTaskBudget(ledger)

	got, err := client.TranscribePage(context.Background(), PageInput{Image: input}, budget)
	if err != nil {
		t.Fatalf("transcribe with repair: %v", err)
	}
	if len(got.Visuals) != 1 || calls.Load() != 2 {
		t.Fatalf("result=%+v calls=%d", got, calls.Load())
	}
	ledger.mu.Lock()
	commits := ledger.commits
	ledger.mu.Unlock()
	if commits != 2 {
		t.Fatalf("initial and repair attempts must both be charged, commits=%d", commits)
	}
	if _, err := client.TranscribePage(context.Background(), PageInput{Image: input}, nil); err != nil {
		t.Fatalf("cache hit should not require budget/provider: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("cache hit called provider, calls=%d", calls.Load())
	}
}

func TestOpenAIResultCacheWriteFailureIsChargedAndReturned(t *testing.T) {
	t.Run("page", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeOpenAIResponse(t, w, `{"markdown":"page text","visuals":[]}`)
		}))
		defer server.Close()
		client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), failingPutResultCache{})
		if err != nil {
			t.Fatal(err)
		}
		input, err := NormalizeImage(context.Background(), testPNG(t, 8, 8), "image/png", client.ImageLimits())
		if err != nil {
			t.Fatal(err)
		}
		input.Scope = CacheScope{UserID: "u_1", KBID: "kb_1", DocID: "doc_1"}
		input.Format = "pdf"
		input.Location = document.SourceLocation{Kind: document.LocationPage, Index: 1, Label: "page 1"}
		ledger := &fakeBudgetLedger{}
		if _, err := client.TranscribePage(context.Background(), PageInput{Image: input}, newFakeTaskBudget(ledger)); !errors.Is(err, errTestCachePut) {
			t.Fatalf("cache failure error=%v", err)
		}
		ledger.mu.Lock()
		commits := ledger.commits
		ledger.mu.Unlock()
		if commits != 1 {
			t.Fatalf("sent page request was not charged, commits=%d", commits)
		}
	})

	t.Run("image", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeOpenAIResponse(t, w, `{"caption":"figure","ocrText":"","kind":"diagram","decorative":false,"confidence":0.8}`)
		}))
		defer server.Close()
		client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), failingPutResultCache{})
		if err != nil {
			t.Fatal(err)
		}
		input, err := NormalizeImage(context.Background(), testPNG(t, 8, 8), "image/png", client.ImageLimits())
		if err != nil {
			t.Fatal(err)
		}
		input.Scope = CacheScope{UserID: "u_1", KBID: "kb_1", DocID: "doc_1"}
		input.Format = "docx"
		input.Location = document.SourceLocation{Kind: document.LocationDocument, Label: "document"}
		ledger := &fakeBudgetLedger{}
		if _, err := client.DescribeImage(context.Background(), input, newFakeTaskBudget(ledger)); !errors.Is(err, errTestCachePut) {
			t.Fatalf("cache failure error=%v", err)
		}
		ledger.mu.Lock()
		commits := ledger.commits
		ledger.mu.Unlock()
		if commits != 1 {
			t.Fatalf("sent image request was not charged, commits=%d", commits)
		}
	})
}

func testFence() store.IndexFence {
	return store.IndexFence{TaskID: 11, DocID: "doc_1", DocVersion: 1, ClaimGeneration: 1, LeaseOwner: "worker"}
}

func TestOpenAIClientRejectsUnsupportedPolicyRedirectAndOversizeGzip(t *testing.T) {
	cfg := config.RAGDocumentAICfg{APIType: "anthropic", Endpoint: "https://example.com/v1", VisionModel: "v", AllowedEndpointHosts: []string{"example.com"}}
	if _, err := NewOpenAICompatible(cfg, documentAILimits(), nil); err == nil {
		t.Fatal("expected apiType rejection")
	}

	t.Run("redirect", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://example.invalid/steal")
			w.WriteHeader(http.StatusFound)
		}))
		defer server.Close()
		client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = callTestImage(t, client)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "redirect") {
			t.Fatalf("redirect error = %v", err)
		}
		var typed *Error
		if !errors.As(err, &typed) || typed.Kind != ErrorPolicy || IsRetryable(err) {
			t.Fatalf("redirect classification = %#v retryable=%v, want non-retryable policy", typed, IsRetryable(err))
		}
	})

	t.Run("decompressed bytes", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write([]byte(strings.Repeat("x", 2048)))
			_ = gz.Close()
		}))
		defer server.Close()
		limits := documentAILimits()
		limits.MaxDocumentAIResponseBytes = 256
		client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), limits, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := callTestImage(t, client); err == nil {
			t.Fatal("expected decompressed response limit")
		}
	})
}

func callTestImage(t *testing.T, client *Client) (ImageDescription, error) {
	t.Helper()
	input, err := NormalizeImage(context.Background(), testPNG(t, 4, 4), "image/png", client.ImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	input.Format = "docx"
	input.Location = document.SourceLocation{Kind: document.LocationDocument, Label: "document"}
	return client.DescribeImage(context.Background(), input, newFakeTaskBudget(&fakeBudgetLedger{}))
}

func TestProviderErrorClassification(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "provider unavailable", status)
			}))
			defer server.Close()
			client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := callTestImage(t, client); err == nil || !IsRetryable(err) {
				t.Fatalf("status %d error = %v, want retryable", status, err)
			}
		})
	}
}

func TestVisionOpenAIResponseEnvelopeDepthAndChoiceLimit(t *testing.T) {
	valid := []byte(`{"choices":[{"message":{"content":"{\"kind\":\"diagram\"}"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	if _, _, err := parseChatResponse(valid, 32, 8); err != nil {
		t.Fatalf("valid response envelope: %v", err)
	}

	multiple := []byte(`{"choices":[{"message":{"content":"first"}},{"message":{"content":"second"}}]}`)
	if _, _, err := parseChatResponse(multiple, 32, 8); err == nil {
		t.Fatal("expected choices array limit rejection")
	}

	deep := `{"choices":[{"message":{"content":"ok"}}],"metadata":` + strings.Repeat(`{"nested":`, 12) + `true` + strings.Repeat(`}`, 12) + `}`
	if _, _, err := parseChatResponse([]byte(deep), 32, 8); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("deep outer response error = %v", err)
	}

	longContent := strings.Repeat("汉", 32)
	withUsage, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": longContent}}},
		"usage":   map[string]any{"prompt_tokens": 2, "completion_tokens": 8},
	})
	if _, usage, err := parseChatResponse(withUsage, 8, 8); err != nil || usage == nil || usage.OutputTokens != 8 {
		t.Fatalf("reported usage should govern token limit: usage=%+v err=%v", usage, err)
	}
	withoutUsage, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"content": longContent}}},
	})
	if _, _, err := parseChatResponse(withoutUsage, 8, 8); err == nil {
		t.Fatal("unreported oversized output bypassed conservative local estimate")
	}
}

func TestVisionOpenAIRequestBodyAndLocationBounds(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeOpenAIResponse(t, w, `{"caption":"figure","ocrText":"","kind":"diagram","decorative":false,"confidence":0.8}`)
	}))
	defer server.Close()
	client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	input, err := NormalizeImage(context.Background(), testPNG(t, 8, 8), "image/png", client.ImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	input.Format = "docx"
	input.Location = document.SourceLocation{
		Kind: document.LocationDocument, Label: strings.Repeat("x", maxSourceLocationLabelBytes+1),
	}
	if _, err := client.DescribeImage(context.Background(), input, newFakeTaskBudget(&fakeBudgetLedger{})); err == nil {
		t.Fatal("expected source location label limit rejection")
	}
	if calls.Load() != 0 {
		t.Fatalf("oversized location reached provider, calls=%d", calls.Load())
	}

	input.Location.Label = "document"
	client.maxRequestBytes = 1
	_, err = client.DescribeImage(context.Background(), input, newFakeTaskBudget(&fakeBudgetLedger{}))
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorPolicy {
		t.Fatalf("request body bound error = %v, want policy", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("oversized request reached provider, calls=%d", calls.Load())
	}
}

func TestVisionOpenAICostEstimatorFeedsDurableReservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeOpenAIResponse(t, w, `{"caption":"figure","ocrText":"","kind":"diagram","decorative":false,"confidence":0.8}`)
	}))
	defer server.Close()
	client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	client.costEstimator = func(model string, inputTokens, outputTokens int64) int64 {
		if model != "vision-test" || inputTokens <= 0 || outputTokens <= 0 {
			t.Fatalf("cost estimator input model=%q input=%d output=%d", model, inputTokens, outputTokens)
		}
		return 777
	}
	input, err := NormalizeImage(context.Background(), testPNG(t, 8, 8), "image/png", client.ImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	input.Format = "docx"
	input.Location = document.SourceLocation{Kind: document.LocationDocument, Label: "document"}
	ledger := &fakeBudgetLedger{}
	if _, err := client.DescribeImage(context.Background(), input, newFakeTaskBudget(ledger)); err != nil {
		t.Fatalf("describe image: %v", err)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.reserve == nil || ledger.reserve.EstimatedCostMicroUSD != 777 {
		t.Fatalf("reserved cost = %+v, want 777 micro-USD", ledger.reserve)
	}
}

func TestVisionTextTokenEstimateIsConservativeForUnicodeAndHighEntropy(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"汉字", "🙂🚀", "a9F/0xDEADBEEF", "mixed汉🙂a9/"} {
		raw := []byte(value)
		if got, want := estimateTextTokens(raw), int64(len(raw)); got != want {
			t.Fatalf("estimateTextTokens(%q)=%d, want byte upper bound %d", value, got, want)
		}
		if got, want := estimateInputTokens(raw, NormalizedImageInput{}), int64(len(raw)); got != want {
			t.Fatalf("estimateInputTokens(%q)=%d, want byte upper bound %d", value, got, want)
		}
	}
}
