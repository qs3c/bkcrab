package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

func TestSelectMetadataDocumentsUsesLargestNewestAndUniformGroups(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	documents := make([]store.RAGDocumentRecord, 16)
	for i := range documents {
		documents[i] = store.RAGDocumentRecord{
			ID: fmt.Sprintf("doc-%02d", i), FileName: fmt.Sprintf("file-%02d.md", i),
			TokenCount: (i + 1) * 100, UploadedAt: base.Add(time.Duration(i) * time.Hour),
		}
	}

	selected := selectMetadataDocuments(documents, 12)
	if len(selected) != 12 {
		t.Fatalf("selected %d documents, want 12: %+v", len(selected), selected)
	}
	seen := map[string]bool{}
	for _, document := range selected {
		if seen[document.ID] {
			t.Fatalf("duplicate selected document %s", document.ID)
		}
		seen[document.ID] = true
	}
	for _, id := range []string{"doc-15", "doc-14", "doc-13", "doc-12"} {
		if !seen[id] {
			t.Fatalf("largest document %s was not selected: %+v", id, selected)
		}
	}
	// The newest group skips the four documents already selected as largest
	// and continues scanning until it contributes four unique documents.
	for _, id := range []string{"doc-11", "doc-10", "doc-09", "doc-08"} {
		if !seen[id] {
			t.Fatalf("newest unique document %s was not selected: %+v", id, selected)
		}
	}
}

func TestMetadataChunkIndexes(t *testing.T) {
	tests := []struct {
		count int
		want  string
	}{
		{count: 0, want: "[]"},
		{count: 1, want: "[0]"},
		{count: 2, want: "[0 1]"},
		{count: 9, want: "[0 4 8]"},
	}
	for _, test := range tests {
		if got := fmt.Sprint(metadataChunkIndexes(test.count)); got != test.want {
			t.Fatalf("metadataChunkIndexes(%d) = %s, want %s", test.count, got, test.want)
		}
	}
}

func TestBuildMetadataSourceUsesOnlyDoneDocumentChunks(t *testing.T) {
	service, fake := newTestService(t, false)
	ctx := context.Background()
	kb, err := service.CreateKB(ctx, "u1", "temporary", "", 512, 64)
	if err != nil {
		t.Fatal(err)
	}
	ready := &store.RAGDocumentRecord{
		ID: "ready", KBID: kb.ID, FileName: "manual.md", Status: "DONE",
		ChunkCount: 5, TokenCount: 500, Version: 2, UploadedAt: time.Now().UTC(),
	}
	if err := service.st.CreateRAGDocument(ctx, ready); err != nil {
		t.Fatal(err)
	}
	if err := service.st.CreateRAGDocument(ctx, &store.RAGDocumentRecord{
		ID: "pending", KBID: kb.ID, FileName: "pending.md", Status: "PROCESSING",
		ChunkCount: 1, TokenCount: 10, Version: 1, UploadedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := fake.UpsertChunks(ctx, kb.ID, []vector.ChunkData{
		{DocID: ready.ID, Index: 0, DocVersion: 2, Content: "BEGIN installation guide", Vector: []float32{1, 0, 0, 0}},
		{DocID: ready.ID, Index: 2, DocVersion: 2, Content: "MIDDLE troubleshooting", Vector: []float32{1, 0, 0, 0}},
		{DocID: ready.ID, Index: 4, DocVersion: 2, Content: "END support workflow", Vector: []float32{1, 0, 0, 0}},
		{DocID: ready.ID, Index: 0, DocVersion: 1, Content: "STALE content", Vector: []float32{1, 0, 0, 0}},
	}); err != nil {
		t.Fatal(err)
	}

	source, err := service.BuildMetadataSource(ctx, "u1", kb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.DocumentCount != 1 || source.SampledDocumentCount != 1 {
		t.Fatalf("source counts = %+v", source)
	}
	if !strings.Contains(source.Catalog, "manual.md") || strings.Contains(source.Catalog, "pending.md") {
		t.Fatalf("catalog includes wrong documents: %q", source.Catalog)
	}
	for _, content := range []string{"BEGIN", "MIDDLE", "END"} {
		if !strings.Contains(source.Excerpts, content) {
			t.Fatalf("excerpts missing %s: %q", content, source.Excerpts)
		}
	}
	if strings.Contains(source.Excerpts, "STALE") {
		t.Fatalf("excerpts included stale document version: %q", source.Excerpts)
	}
	if _, err := service.BuildMetadataSource(ctx, "u2", kb.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-owner metadata source error = %v, want ErrForbidden", err)
	}
}

func TestBuildMetadataSourceRejectsKnowledgeBaseWithoutDoneDocuments(t *testing.T) {
	service, _ := newTestService(t, false)
	kb, err := service.CreateKB(context.Background(), "u1", "empty", "", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.BuildMetadataSource(context.Background(), "u1", kb.ID); !errors.Is(err, ErrNoReadyDocuments) {
		t.Fatalf("error = %v, want ErrNoReadyDocuments", err)
	}
}
