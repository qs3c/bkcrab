package rag

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/fairqueue"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestFairQueueIntegrationClaimCapacityAndDuplicateDelivery(t *testing.T) {
	var mu sync.Mutex
	var events []fairqueue.TelemetryEvent
	sink := fairqueue.TelemetrySinkFunc(func(_ context.Context, event fairqueue.TelemetryEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	claimCall := 0
	source := &fakeRAGFairQueueSource{claim: func(context.Context, int64, string, int64, string, time.Duration, store.RAGFairQueueClaimLimits) (store.RAGFairQueueClaimResult, error) {
		claimCall++
		if claimCall == 1 {
			return store.RAGFairQueueClaimResult{Disposition: store.RAGFairQueueClaimCapacityDeferred}, nil
		}
		return store.RAGFairQueueClaimResult{Disposition: store.RAGFairQueueClaimDuplicateStale}, nil
	}}
	adapter, err := NewRAGFairQueueAdapter(
		source, &fakeRAGFairQueueRunner{},
		&fakeRAGFairQueueAdmin{
			identity: store.FairQueueWriterIdentity{Fingerprint: testRAGWriter},
			report:   store.RAGFairQueueContractReport{ExpandSchemaReady: true, Contracted: true},
		},
		&fakeRAGFairQueueJournal{session: &fakeRAGFairQueueJournalSession{}},
		RAGFairQueueAdapterOptions{
			WorkerID: "rag-integration", LeaseDuration: time.Minute,
			ClaimLimits: store.RAGFairQueueClaimLimits{GlobalConcurrency: 4, PerUserBurstConcurrency: 4},
			Telemetry:   sink,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testRAGPrepareRequest(t, testRAGMessage(42, 7, testRAGUser))
	prepared, first, err := adapter.Prepare(context.Background(), request)
	if err != nil || prepared != nil || first.Disposition != fairqueue.PrepareCapacityDeferred || first.DeliveryAction != fairqueue.DeliveryNackRequeue {
		t.Fatalf("capacity prepare=(%T,%#v,%v)", prepared, first, err)
	}
	prepared, second, err := adapter.Prepare(context.Background(), request)
	if err != nil || prepared != nil || second.Disposition != fairqueue.PrepareDuplicateStaleTerminal || second.DeliveryAction != fairqueue.DeliveryAckRelease {
		t.Fatalf("duplicate prepare=(%T,%#v,%v)", prepared, second, err)
	}
	mu.Lock()
	defer mu.Unlock()
	var lockEvents, capacityDenied, claimEvents int
	for _, event := range events {
		switch event.Name {
		case fairqueue.TelemetryClaimLock:
			lockEvents++
		case fairqueue.TelemetryClaimCapacity:
			if event.Outcome == "denied" {
				capacityDenied++
			}
		case fairqueue.TelemetryTaskClaim:
			claimEvents++
		}
	}
	if lockEvents != 2 || capacityDenied != 1 || claimEvents != 2 {
		t.Fatalf("telemetry lock=%d capacity-denied=%d claims=%d events=%#v", lockEvents, capacityDenied, claimEvents, events)
	}
}
