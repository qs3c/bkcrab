package vector

import (
	"context"
	"testing"
)

func TestFakeGetChunksReturnsExactVersionsAndStableOrder(t *testing.T) {
	ctx := context.Background()
	fake := NewFake()
	if err := fake.EnsureCollection(ctx, "kb", 2); err != nil {
		t.Fatal(err)
	}
	if err := fake.UpsertChunks(ctx, "kb", []ChunkData{
		{DocID: "b", Index: 2, DocVersion: 1, Content: "b2", Vector: []float32{1, 0}},
		{DocID: "a", Index: 0, DocVersion: 1, Content: "old", Vector: []float32{1, 0}},
		{DocID: "a", Index: 0, DocVersion: 2, Content: "current", Vector: []float32{1, 0}},
	}); err != nil {
		t.Fatal(err)
	}

	chunks, err := fake.GetChunks(ctx, "kb", []ChunkRef{
		{DocID: "b", Index: 2, DocVersion: 1},
		{DocID: "a", Index: 0, DocVersion: 2},
		{DocID: "missing", Index: 0, DocVersion: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 2 {
		t.Fatalf("chunks = %+v, want 2 exact matches", chunks)
	}
	if chunks[0].DocID != "a" || chunks[0].DocVersion != 2 || chunks[0].Content != "current" {
		t.Fatalf("first chunk = %+v, want current a version", chunks[0])
	}
	if chunks[1].DocID != "b" || chunks[1].Index != 2 {
		t.Fatalf("second chunk = %+v, want b/2", chunks[1])
	}
}
