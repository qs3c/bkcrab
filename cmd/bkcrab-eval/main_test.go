package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/workspace"
)

func TestTaskWorkspaceAllScopesUseTaskRoot(t *testing.T) {
	root := t.TempDir()
	w := &taskWorkspace{LocalFS: workspace.NewLocalFS(root), name: "task"}
	ctx := context.Background()
	if err := w.Put(ctx, "agent", "project", "session", "result.txt", strings.NewReader("ready\n"), 6, "text/plain"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "task", "result.txt"))
	if err != nil || string(data) != "ready\n" {
		t.Fatalf("task root write: %q %v", data, err)
	}
	r, err := w.Get(ctx, "other", "other", "other", "result.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err = io.ReadAll(r)
	if err != nil || string(data) != "ready\n" {
		t.Fatalf("scope mapping: %q %v", data, err)
	}
	dir, ok := w.LocalScopeDir("a", "p", "s")
	if !ok || dir != filepath.Join(root, "task") {
		t.Fatalf("shell root %q", dir)
	}
	if _, err := w.Stat(ctx, "a", "p", "s", "result.txt"); err != nil {
		t.Fatal(err)
	}
	if err := w.Delete(ctx, "a", "p", "s", "result.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "task", "result.txt")); !os.IsNotExist(err) {
		t.Fatal("file not removed")
	}
}

func TestEvidenceRedactsAndPreservesJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink, err := newEvidence(path, "test-api-key", "test-search-key")
	if err != nil {
		t.Fatal(err)
	}
	sink.write("test", map[string]string{"value": "test-api-key test-search-key"})
	sink.file.Close()
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "test-api-key") || strings.Contains(string(data), "test-search-key") || !json.Valid(data) {
		t.Fatalf("bad evidence %s", data)
	}
}

type failingStream struct{}

func (failingStream) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	return nil, errors.New("fixture failure")
}
func (failingStream) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	ch := make(chan provider.StreamChunk)
	r := provider.NewStreamReader(ch)
	r.SetErr(errors.New("fixture stream failure"))
	close(ch)
	return r, nil
}
func TestStreamErrorPropagates(t *testing.T) {
	sink, err := newEvidence(filepath.Join(t.TempDir(), "events.jsonl"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.file.Close()
	p := &tracedProvider{inner: failingStream{}, sink: sink}
	r, err := p.ChatStream(context.Background(), nil, nil, "fixture", 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Next(); ok {
		t.Fatal("unexpected chunk")
	}
	if r.Err() == nil || !strings.Contains(r.Err().Error(), "fixture stream failure") {
		t.Fatalf("lost failure %v", r.Err())
	}
}
