package vision

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/store"
)

// TestVisionProviderIntegration performs one bounded, non-sensitive visual
// request against the explicitly configured OpenAI-compatible provider. The
// normal unit suite never enables this gate and therefore never incurs cost.
func TestVisionProviderIntegration(t *testing.T) {
	if os.Getenv("RAG_VISION_INTEGRATION") != "1" {
		t.Skip("RAG_VISION_INTEGRATION=1 is required for the real VLM test")
	}

	cfg := integrationVisionConfig(t)
	limits := config.RAGLimitsCfg{
		MaxAssetBytes: 1 << 20, MaxVisionInputBytes: 1 << 20,
		MaxImagePixels: 1_000_000, DisplayMaxEdge: 480,
		MaxDocumentAIResponseBytes: 64 << 10, MaxDocumentAIOutputTokens: 512,
		MaxDocumentAIJSONDepth: 16,
	}
	// A nil cache is intentional: a passing integration run must cross the
	// network boundary rather than reuse a previous typed result.
	client, err := NewOpenAICompatible(cfg, limits, nil)
	if err != nil {
		t.Fatalf("create real VLM client: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join("..", "testdata", "multimodal", "diagram.png"))
	if err != nil {
		t.Fatalf("read CC0 integration image: %v", err)
	}
	input, err := NormalizeImage(context.Background(), raw, "image/png", client.ImageLimits())
	if err != nil {
		t.Fatalf("normalize integration image: %v", err)
	}
	input.Format = "pdf"
	input.Location = document.SourceLocation{Kind: document.LocationPage, Index: 1, Label: "integration page 1"}
	input.AltText = "A small three-stage processing-flow diagram"
	input.Scope = CacheScope{
		UserID: "u_vision_integration", KBID: "kb_vision_integration",
		DocID: "doc_vision_integration", ParseFingerprint: strings.Repeat("a", 64),
	}

	ledger := &fakeBudgetLedger{}
	fence := store.IndexFence{
		TaskID: 1, DocID: input.Scope.DocID, DocVersion: 1,
		ClaimGeneration: 1, LeaseOwner: "vision-integration",
	}
	// At most two calls are authorized (initial + one schema repair), with a
	// small token and USD 0.05 task cap for this single public-domain fixture.
	budget := NewTaskDocumentAIBudget(ledger, TaskBudgetConfig{
		Fence: fence, UserID: input.Scope.UserID, ReservationTTL: time.Minute,
		TaskLimits: store.RAGDocumentAILimits{
			MaxRequests: 2, MaxTokens: 20_000, MaxCostMicroUSD: 50_000,
		},
		UserLimits: store.RAGDocumentAILimits{
			MaxRequests: 2, MaxTokens: 20_000, MaxCostMicroUSD: 50_000,
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(cfg.TimeoutMS)*time.Millisecond)
	defer cancel()
	got, err := client.DescribeImage(ctx, input, budget)
	if err != nil {
		t.Fatalf("describe integration image with real VLM: %v", err)
	}
	if err := got.Validate(DefaultSchemaLimits()); err != nil {
		t.Fatalf("real VLM bypassed the strict typed schema: %v", err)
	}
	if strings.TrimSpace(got.Caption) == "" && strings.TrimSpace(got.OCRText) == "" {
		t.Fatalf("real VLM returned no semantic text: %+v", got)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.createCalls != 1 || ledger.commits < 1 || ledger.commits > 2 || ledger.reserve == nil ||
		ledger.reserveFence != fence || ledger.markFence != fence {
		t.Fatalf("real VLM did not use the bounded fenced budget: create=%d commits=%d reserve=%+v reserveFence=%+v markFence=%+v",
			ledger.createCalls, ledger.commits, ledger.reserve, ledger.reserveFence, ledger.markFence)
	}
}

func integrationVisionConfig(t *testing.T) config.RAGDocumentAICfg {
	t.Helper()
	endpoint := strings.TrimSpace(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_ENDPOINT"))
	model := strings.TrimSpace(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_VISION_MODEL"))
	allowedHosts := splitIntegrationHosts(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_ALLOWED_ENDPOINT_HOSTS"))
	if endpoint == "" || model == "" || len(allowedHosts) == 0 {
		t.Fatal("RAG_VISION_INTEGRATION=1 requires BKCRAB_RAG_DOCUMENT_AI_ENDPOINT, BKCRAB_RAG_DOCUMENT_AI_VISION_MODEL, and BKCRAB_RAG_DOCUMENT_AI_ALLOWED_ENDPOINT_HOSTS")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		t.Fatalf("invalid BKCRAB_RAG_DOCUMENT_AI_ENDPOINT %q", endpoint)
	}
	allowed := false
	for _, host := range allowedHosts {
		if canonicalHost(host) == canonicalHost(parsed.Hostname()) {
			allowed = true
			break
		}
	}
	if !allowed {
		t.Fatalf("DocumentAI endpoint host %q is not in BKCRAB_RAG_DOCUMENT_AI_ALLOWED_ENDPOINT_HOSTS", parsed.Hostname())
	}

	timeoutMS := integrationPositiveInt(t, "BKCRAB_RAG_DOCUMENT_AI_TIMEOUT_MS", 120_000)
	allowPrivate := false
	if raw := strings.TrimSpace(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_ALLOW_PRIVATE_ENDPOINT")); raw != "" {
		allowPrivate, err = strconv.ParseBool(raw)
		if err != nil {
			t.Fatalf("BKCRAB_RAG_DOCUMENT_AI_ALLOW_PRIVATE_ENDPOINT must be true or false, got %q", raw)
		}
	}
	apiType := strings.TrimSpace(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_API_TYPE"))
	if apiType == "" {
		apiType = "openai-compatible"
	}
	return config.RAGDocumentAICfg{
		APIType: apiType, Endpoint: endpoint,
		APIKey:      strings.TrimSpace(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_API_KEY")),
		VisionModel: model, TimeoutMS: timeoutMS, VisionConcurrency: 1,
		VisionPromptVersion:  strings.TrimSpace(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_VISION_PROMPT_VERSION")),
		AllowedEndpointHosts: allowedHosts, AllowPrivateEndpoint: allowPrivate,
	}
}

func splitIntegrationHosts(raw string) []string {
	values := strings.Split(raw, ",")
	hosts := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			hosts = append(hosts, value)
		}
	}
	return hosts
}

func integrationPositiveInt(t *testing.T, name string, fallback int) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		t.Fatalf("%s must be a positive integer, got %q", name, raw)
	}
	return value
}
