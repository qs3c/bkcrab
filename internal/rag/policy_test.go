package rag

import (
	"context"
	"math"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
)

func TestRuntimePolicySnapshotIsImmutablePerRead(t *testing.T) {
	first := config.RAGRuntimePolicyData{Version: 1, TopN: 5, CandidateTopK: 20, MinScore: .5, Temperature: .2, MaxTokens: 100, RAGPromptBundleVersion: "v1"}
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

func TestSearchWithOptionsRejectsInvalidMinScore(t *testing.T) {
	value := math.NaN()
	_, _, err := (&Service{}).SearchWithOptions(context.Background(), "u", nil, SearchContext{Query: "q"}, SearchOptions{MinScore: &value})
	if err == nil {
		t.Fatal("NaN minScore accepted")
	}
}
