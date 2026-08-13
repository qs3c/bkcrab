package rag

import (
	"context"
	"math"
	"sync"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
)

func TestRuntimePolicySnapshotIsImmutablePerRead(t *testing.T) {
	first := config.RAGRuntimePolicyData{Version: 1, TopN: 5, CandidateTopK: 20, MinScore: .5, Temperature: .2, MaxTokens: 100, RAGPromptBundleVersion: RAGAnswerPromptBundleV1}
	snapshot, err := NewRuntimePolicySnapshot(first)
	if err != nil {
		t.Fatal(err)
	}
	captured := snapshot.Current()
	second := first
	second.Version = 2
	second.MinScore = .7
	if err := snapshot.Publish(second); err != nil {
		t.Fatal(err)
	}
	if captured.Version != 1 || captured.MinScore != .5 {
		t.Fatal("captured policy changed")
	}
	if current := snapshot.Current(); current.Version != 2 || current.MinScore != .7 {
		t.Fatalf("current=%+v", current)
	}
}

func TestRuntimePolicyVersionOnePreservesProductionDefaults(t *testing.T) {
	cfg := config.RAGCfg{Reranker: config.RAGRerankerCfg{CandidateTopK: 2, MinScore: .7}}
	got := DefaultRuntimePolicy(cfg)
	if got.Version != 1 || got.TopN != 5 || got.CandidateTopK != 5 || got.MinScore != .7 ||
		got.Temperature != .2 || got.MaxTokens != 4096 || got.RAGPromptBundleVersion != RAGAnswerPromptBundleV1 {
		t.Fatalf("version one policy = %+v", got)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("version one policy invalid: %v", err)
	}
}

func TestRuntimePolicyConcurrentReadsObserveOnlyCompleteSnapshots(t *testing.T) {
	initial := config.RAGRuntimePolicyData{
		Version: 1, TopN: 1, CandidateTopK: 11, MinScore: .1,
		Temperature: .2, MaxTokens: 100, RAGPromptBundleVersion: RAGAnswerPromptBundleV1,
	}
	provider, err := NewRuntimePolicySnapshot(initial)
	if err != nil {
		t.Fatal(err)
	}
	var readers sync.WaitGroup
	errorsSeen := make(chan config.RAGRuntimePolicyData, 1)
	for worker := 0; worker < 16; worker++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for iteration := 0; iteration < 2000; iteration++ {
				got := provider.Current()
				if got.CandidateTopK != int(got.Version)+10 || got.MaxTokens != int(got.Version)+99 ||
					got.RAGPromptBundleVersion != RAGAnswerPromptBundleV1 {
					select {
					case errorsSeen <- got:
					default:
					}
					return
				}
			}
		}()
	}
	for version := int64(2); version <= 100; version++ {
		next := initial
		next.Version = version
		next.CandidateTopK = int(version) + 10
		next.MaxTokens = int(version) + 99
		if err := provider.Publish(next); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	select {
	case torn := <-errorsSeen:
		t.Fatalf("reader observed torn runtime policy: %+v", torn)
	default:
	}
}

func TestRuntimePolicyRejectsInvalidRevisionWithoutReplacingCurrent(t *testing.T) {
	initial := DefaultRuntimePolicy(config.RAGCfg{})
	provider, err := NewRuntimePolicySnapshot(initial)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*config.RAGRuntimePolicyData){
		"zero version":   func(value *config.RAGRuntimePolicyData) { value.Version = 0 },
		"invalid bounds": func(value *config.RAGRuntimePolicyData) { value.CandidateTopK = value.TopN - 1 },
		"unknown prompt": func(value *config.RAGRuntimePolicyData) { value.RAGPromptBundleVersion = "uploaded-prompt" },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := initial
			mutate(&invalid)
			if err := provider.Publish(invalid); err == nil {
				t.Fatal("invalid runtime policy was accepted")
			}
			if got := provider.Current(); got != initial {
				t.Fatalf("invalid publish replaced current: %+v", got)
			}
		})
	}
}

func TestRuntimePolicyCapturedRequestDoesNotChangeMidFlight(t *testing.T) {
	service, fake, kbID := newRAGSearchOptionsFixture(t)
	provider, ok := service.RuntimePolicyProvider().(*RuntimePolicySnapshot)
	if !ok {
		t.Fatalf("runtime policy provider = %T", service.RuntimePolicyProvider())
	}
	ctx, captured := service.CaptureRuntimePolicy(context.Background())
	if captured.Version != 1 || captured.CandidateTopK != 20 {
		t.Fatalf("initial capture = %+v", captured)
	}
	next := captured
	next.Version = 2
	next.TopN = 1
	next.CandidateTopK = 2
	next.MinScore = .9
	if err := provider.Publish(next); err != nil {
		t.Fatal(err)
	}
	capture := &queryCaptureVector{Fake: fake}
	service.vec = capture
	hits, oldTrace, err := service.SearchWithOptions(ctx, "u1", []string{kbID}, SearchContext{Query: "安装权限"}, SearchOptions{
		Reranker: boolPointer(false),
	})
	if err != nil || len(hits) != 2 {
		t.Fatalf("captured request hits=%+v err=%v", hits, err)
	}
	_, oldTopKs := capture.routes()
	if oldTrace.RuntimePolicyVersion != 1 || oldTrace.TopN != 5 || oldTrace.CandidateTopK != 20 || oldTrace.MinScore != captured.MinScore ||
		len(oldTopKs) != 1 || oldTopKs[0] != 20 {
		t.Fatalf("captured request changed after publish: trace=%+v topKs=%v", oldTrace, oldTopKs)
	}

	freshCapture := &queryCaptureVector{Fake: fake}
	service.vec = freshCapture
	freshHits, freshTrace, err := service.SearchWithOptions(context.Background(), "u1", []string{kbID}, SearchContext{Query: "安装权限"}, SearchOptions{
		Reranker: boolPointer(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, freshTopKs := freshCapture.routes()
	if len(freshHits) != 1 || freshTrace.RuntimePolicyVersion != 2 || freshTrace.TopN != 1 || freshTrace.CandidateTopK != 2 || freshTrace.MinScore != .9 ||
		len(freshTopKs) != 1 || freshTopKs[0] != 2 {
		t.Fatalf("fresh request missed published policy: trace=%+v topKs=%v", freshTrace, freshTopKs)
	}
}

func TestSearchWithOptionsRejectsInvalidMinScore(t *testing.T) {
	value := math.NaN()
	_, _, err := (&Service{}).SearchWithOptions(context.Background(), "u", nil, SearchContext{Query: "q"}, SearchOptions{MinScore: &value})
	if err == nil {
		t.Fatal("NaN minScore accepted")
	}
}
