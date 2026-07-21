package vision

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/store"
)

type quotaRejectingLedger struct{ fakeBudgetLedger }

func (*quotaRejectingLedger) ReserveRAGDocumentAIUsage(
	context.Context,
	store.IndexFence,
	*store.RAGDocumentAIUsageRecord,
	store.RAGDocumentAILimits,
) (bool, error) {
	return false, store.ErrRAGDocumentAIBudgetExceeded
}

type visionTelemetryCollector struct {
	mu     sync.Mutex
	events []telemetry.Event
}

func (c *visionTelemetryCollector) Record(_ context.Context, event telemetry.Event) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *visionTelemetryCollector) snapshot() []telemetry.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]telemetry.Event(nil), c.events...)
}

func TestDocumentAITelemetryRecordsCacheCallsUsageAndDurableTransitions(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeOpenAIResponse(t, w, `{"kind":"diagram","caption":"flow","ocrText":"A to B","decorative":false,"confidence":0.9}`)
	}))
	defer server.Close()

	cache := NewMemoryCache(DefaultSchemaLimits())
	client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), cache)
	if err != nil {
		t.Fatal(err)
	}
	collector := &visionTelemetryCollector{}
	client.SetRecorder(collector)

	input, err := NormalizeImage(context.Background(), testPNG(t, 12, 12), "image/png", client.ImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	input.Scope = CacheScope{UserID: "u_observe", KBID: "kb_observe", DocID: "doc_observe"}
	input.Format = "pdf"
	ledger := &fakeBudgetLedger{}
	fence := store.IndexFence{TaskID: 71, DocID: "doc_observe", DocVersion: 4, ClaimGeneration: 2, LeaseOwner: "worker-observe"}
	budget := NewTaskDocumentAIBudget(ledger, TaskBudgetConfig{
		Fence: fence, UserID: "u_observe", Recorder: collector,
		TaskLimits:     store.RAGDocumentAILimits{MaxRequests: 10, MaxTokens: 10_000},
		UserLimits:     store.RAGDocumentAILimits{MaxRequests: 100, MaxTokens: 100_000},
		ReservationTTL: time.Minute,
	})

	for range 2 {
		if _, err := client.DescribeImage(context.Background(), input, budget); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d want=1 after cache hit", calls.Load())
	}

	events := collector.snapshot()
	var cacheMiss, cacheHit, providerCall bool
	transitions := map[string]bool{}
	for _, event := range events {
		fields := event.Fields
		if fields.DocID != "doc_observe" {
			t.Fatalf("unexpected correlation fields: %+v", fields)
		}
		switch event.Name {
		case telemetry.EventResultCache:
			cacheMiss = cacheMiss || fields.CacheStatus == "miss"
			cacheHit = cacheHit || fields.CacheStatus == "hit"
		case telemetry.EventDocumentAICall:
			providerCall = providerCall || fields.Operation == OperationImage && fields.RequestCount == 1 &&
				fields.InputTokens == 12 && fields.OutputTokens == 8 && fields.Outcome == "ok"
		case telemetry.EventDocumentAIBudget:
			transitions[fields.Transition] = true
		}
	}
	if !cacheMiss || !cacheHit || !providerCall || !transitions["reserve"] || !transitions["sent"] || !transitions["commit"] {
		t.Fatalf("missing telemetry cacheMiss=%v cacheHit=%v call=%v transitions=%v events=%+v",
			cacheMiss, cacheHit, providerCall, transitions, events)
	}
}

func TestDocumentAITelemetryRecordsRepairAndQuotaRejection(t *testing.T) {
	collector := &visionTelemetryCollector{}
	fence := store.IndexFence{TaskID: 72, DocID: "doc_repair_observe", DocVersion: 5, ClaimGeneration: 1, LeaseOwner: "worker-observe"}
	rejectedBudget := NewTaskDocumentAIBudget(&quotaRejectingLedger{}, TaskBudgetConfig{
		Fence: fence, UserID: "u_observe", Recorder: collector,
		TaskLimits: store.RAGDocumentAILimits{MaxRequests: 1, MaxTokens: 1},
		UserLimits: store.RAGDocumentAILimits{MaxRequests: 1, MaxTokens: 1},
	})
	_, err := rejectedBudget.Reserve(context.Background(), fence, AttemptRequest{
		LogicalRequestKey: "quota-logical", Operation: OperationPage,
		ProviderFingerprint: "provider", InputTokens: 1, OutputTokens: 1,
	})
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorBudget {
		t.Fatalf("quota error=%v", err)
	}

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			writeOpenAIResponse(t, w, `{"kind":"diagram","caption":123}`)
			return
		}
		writeOpenAIResponse(t, w, `{"kind":"diagram","caption":"flow","ocrText":"A to B","decorative":false,"confidence":0.9}`)
	}))
	defer server.Close()
	client, err := NewOpenAICompatible(documentAIConfigForServer(t, server.URL), documentAILimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	client.SetRecorder(collector)
	input, err := NormalizeImage(context.Background(), testPNG(t, 12, 12), "image/png", client.ImageLimits())
	if err != nil {
		t.Fatal(err)
	}
	input.Scope = CacheScope{UserID: "u_observe", KBID: "kb_observe", DocID: fence.DocID}
	input.Format = "docx"
	budget := NewTaskDocumentAIBudget(&fakeBudgetLedger{}, TaskBudgetConfig{
		Fence: fence, UserID: "u_observe", Recorder: collector,
		TaskLimits: store.RAGDocumentAILimits{MaxRequests: 10, MaxTokens: 100_000},
		UserLimits: store.RAGDocumentAILimits{MaxRequests: 100, MaxTokens: 1_000_000},
	})
	if _, err := client.DescribeImage(context.Background(), input, budget); err != nil {
		t.Fatal(err)
	}

	var quota, initial, repair bool
	for _, event := range collector.snapshot() {
		switch event.Name {
		case telemetry.EventDocumentAIBudget:
			quota = quota || event.Fields.Transition == "reserve" && event.Fields.Outcome == "rejected" &&
				event.Fields.ErrorCode == "quota_exceeded"
		case telemetry.EventDocumentAICall:
			initial = initial || event.Fields.Operation == OperationImage
			repair = repair || event.Fields.Operation == OperationImageRepair
		}
	}
	if !quota || !initial || !repair || calls.Load() != 2 {
		t.Fatalf("quota=%v initial=%v repair=%v calls=%d events=%+v", quota, initial, repair, calls.Load(), collector.snapshot())
	}
}
