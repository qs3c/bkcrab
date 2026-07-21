package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/rag/vision"
	"github.com/qs3c/bkcrab/internal/store"
)

type enrichTelemetryCollector struct {
	mu     sync.Mutex
	events []telemetry.Event
}

func (c *enrichTelemetryCollector) Record(_ context.Context, event telemetry.Event) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *enrichTelemetryCollector) snapshot() []telemetry.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]telemetry.Event(nil), c.events...)
}

func TestEnrichmentTelemetryRecordsProviderUsageCacheAndBatchCounts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeOpenAIResponse(t, w, validTableOutput())
	}))
	defer server.Close()

	cache := NewMemoryCache(DefaultSchemaLimits())
	client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), cache)
	if err != nil {
		t.Fatal(err)
	}
	collector := &enrichTelemetryCollector{}
	client.SetRecorder(collector)
	fence := store.IndexFence{TaskID: 81, DocID: "doc_enrich_observe", DocVersion: 2, ClaimGeneration: 3, LeaseOwner: "worker-observe"}
	budget := vision.NewTaskDocumentAIBudget(&testBudgetLedger{}, vision.TaskBudgetConfig{
		Fence: fence, UserID: "user-observe", Recorder: collector, ReservationTTL: time.Minute,
		TaskLimits: store.RAGDocumentAILimits{MaxRequests: 10, MaxTokens: 10_000},
		UserLimits: store.RAGDocumentAILimits{MaxRequests: 100, MaxTokens: 100_000},
	})
	block := EnrichableBlock{
		Kind: BlockTable, RawContent: "| region | capacity |\n|---|---|\n| east | 8 GiB |",
		TokenBudget: 256, ByteBudget: 4096,
		Scope: CacheScope{UserID: "user-observe", KBID: "kb-observe", DocID: fence.DocID},
	}
	for range 2 {
		if _, err := client.Enrich(context.Background(), block, budget); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d want=1", calls.Load())
	}

	processor := NewProcessor(&recordingEnricher{})
	processor.SetRecorder(collector)
	_, warnings := processor.EnrichChunks(context.Background(), processChunks(), ProcessConfig{
		SystemEnabled: true, TextModel: "text-test", KBEnabled: true, MaxBlocks: 1,
		Finalize: FinalizeConfig{ChunkSize: 256, MaxSearchContentBytes: 4096, CollectionMaxLength: 4096},
		Scope:    CacheScope{UserID: "user-observe", KBID: "kb-observe", DocID: fence.DocID},
	}, budget)
	if len(warnings) != 1 {
		t.Fatalf("warnings=%+v want one block-limit warning", warnings)
	}

	var cacheMiss, cacheHit, providerCall, batch bool
	for _, event := range collector.snapshot() {
		fields := event.Fields
		switch event.Name {
		case telemetry.EventResultCache:
			cacheMiss = cacheMiss || fields.CacheKind == "enrichment" && fields.CacheStatus == "miss"
			cacheHit = cacheHit || fields.CacheKind == "enrichment" && fields.CacheStatus == "hit"
		case telemetry.EventDocumentAICall:
			providerCall = providerCall || fields.Operation == vision.OperationEnrichment && fields.InputTokens == 20 &&
				fields.OutputTokens == 40 && fields.RequestCount == 1 && fields.Outcome == "ok"
		case telemetry.EventEnrichmentBatch:
			batch = fields.BlockCount == 2 && fields.SuccessCount == 1 && fields.SkippedCount == 1 && fields.WarningCount == 1
		}
	}
	if !cacheMiss || !cacheHit || !providerCall || !batch {
		t.Fatalf("missing telemetry miss=%v hit=%v call=%v batch=%v events=%+v",
			cacheMiss, cacheHit, providerCall, batch, collector.snapshot())
	}
}
