package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag"
)

func TestFrozenInputsRejectDuplicates(t *testing.T) {
	s := sample{ID: "q", Query: "question", Candidates: []candidate{{ID: "a", SearchText: "text", Hit: rag.Hit{Content: "text"}}}}
	for _, cases := range [][]sample{{s, s}, {{ID: "q", Query: "question", Candidates: append(s.Candidates, s.Candidates...)}}} {
		path := filepath.Join(t.TempDir(), "input.json")
		b, _ := json.Marshal(frozen{Cases: cases})
		os.WriteFile(path, b, 0600)
		if _, _, err := load(path); err == nil {
			t.Fatal("accepted duplicate input")
		}
	}
}
func TestResumePreservesFailuresAndRejectsConfigurationDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.jsonl")
	f, _, err := openOutput(path, "one")
	if err != nil {
		t.Fatal(err)
	}
	r := record{Kind: "rank", Manifest: "one", CaseID: "q", Arm: "jev", Status: "error", Phase: "test"}
	json.NewEncoder(f).Encode(r)
	f.Close()
	f, done, err := openOutput(path, "one")
	if err != nil || !done[key(r)] {
		t.Fatalf("failed attempt would be silently retried: %v", err)
	}
	f.Close()
	if f, _, err := openOutput(path, "two"); err == nil {
		f.Close()
		t.Fatal("accepted incompatible output")
	}
}
