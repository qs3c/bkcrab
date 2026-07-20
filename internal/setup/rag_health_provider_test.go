package setup

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
)

type cachedHealthProvider struct {
	calls    atomic.Int64
	snapshot config.RAGParserHealthSnapshot
}

func (p *cachedHealthProvider) RAGParserHealthSnapshot() config.RAGParserHealthSnapshot {
	p.calls.Add(1)
	return p.snapshot
}

func TestRAGParserHealthProviderIsConcurrentAndReturnsDefensiveSnapshots(t *testing.T) {
	server := NewServer(0)
	provider := &cachedHealthProvider{snapshot: config.RAGParserHealthSnapshot{
		ProtocolVersion: "rag-parser/v1",
		Healthy:         true,
		CheckedAt:       time.Now().UTC(),
		ExpiresAt:       time.Now().Add(time.Minute),
		Office: config.RAGParserOfficeSnapshot{
			Enabled: true, Formats: []string{"docx", "pptx", "xlsx"},
		},
	}}
	server.SetRAGParserHealthProvider(provider)

	const readers = 32
	var wait sync.WaitGroup
	wait.Add(readers)
	for index := 0; index < readers; index++ {
		go func() {
			defer wait.Done()
			snapshot := server.ragParserHealthSnapshot()
			if !snapshot.Healthy || len(snapshot.Office.Formats) != 3 {
				t.Errorf("snapshot=%+v", snapshot)
				return
			}
			snapshot.Office.Formats[0] = "mutated"
		}()
	}
	wait.Wait()

	if got := provider.calls.Load(); got != readers {
		t.Fatalf("provider calls=%d, want %d cached reads", got, readers)
	}
	if got := server.ragParserHealthSnapshot().Office.Formats[0]; got != "docx" {
		t.Fatalf("provider snapshot was mutated through handler copy: %q", got)
	}
}
