package enrich

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
)

// TestEnrichmentProviderIntegration performs one bounded, non-sensitive text
// request against the explicitly configured OpenAI-compatible provider. The
// normal unit suite never enables this gate and therefore never incurs cost.
func TestEnrichmentProviderIntegration(t *testing.T) {
	if os.Getenv("RAG_ENRICH_INTEGRATION") != "1" {
		t.Skip("RAG_ENRICH_INTEGRATION=1 is required for the real text-model test")
	}

	endpoint := strings.TrimSpace(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_ENDPOINT"))
	model := strings.TrimSpace(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_TEXT_MODEL"))
	if endpoint == "" || model == "" {
		t.Fatal("real enrichment test requires DocumentAI endpoint and text model")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" {
		t.Fatalf("invalid DocumentAI endpoint %q", endpoint)
	}

	cfg := config.RAGDocumentAICfg{
		APIType:                 "openai-compatible",
		Endpoint:                endpoint,
		APIKey:                  strings.TrimSpace(os.Getenv("BKCRAB_RAG_DOCUMENT_AI_API_KEY")),
		TextModel:               model,
		TimeoutMS:               120_000,
		EnrichmentConcurrency:   1,
		EnrichmentPromptVersion: "enrichment-integration-v1",
		AllowedEndpointHosts:    []string{parsed.Hostname()},
	}
	client, err := NewOpenAICompatible(cfg, documentAILimits(), nil)
	if err != nil {
		t.Fatalf("create real enrichment client: %v", err)
	}

	block := EnrichableBlock{
		Kind:        BlockTable,
		RawContent:  "| service | requests |\n|---|---:|\n| search | 120 |\n| ingest | 45 |",
		TokenBudget: 512,
		ByteBudget:  8 << 10,
		Scope: CacheScope{
			UserID: "u_enrichment_integration",
			KBID:   "kb_enrichment_integration",
			DocID:  "doc_enrichment_integration",
		},
	}
	ledger := &testBudgetLedger{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	got, err := client.Enrich(ctx, block, testTaskBudget(ledger))
	if err != nil {
		t.Fatalf("enrich integration table with real text model: %v", err)
	}
	if got.Table == nil || strings.TrimSpace(got.Table.Summary) == "" {
		t.Fatalf("real text model returned no typed table summary: %+v", got)
	}
}
