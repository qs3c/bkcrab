package split

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

func TestImageOccurrenceAttachesOnlyToLocalAtomicChunk(t *testing.T) {
	t.Parallel()
	location := document.SourceLocation{Kind: document.LocationPage, Index: 3, Label: "第 3 页"}
	artifact := imageArtifact(location, "before short.\n\n![alt](rag-asset://occ_1)\n\nafter short.\n\n"+strings.Repeat("unrelated long paragraph. ", 40), []document.ArtifactOccurrence{{
		ID: "occ_1", AssetID: "ast_one", UnitID: "unit_1", Order: 4, Location: location,
		Caption: "系统架构图", OCRText: "Gateway -> Retriever", Confidence: 0.9,
	}})
	chunks := Split(artifact, Config{ChunkSize: 80, ChunkOverlap: 10})
	var imageIndexes []int
	for i, chunk := range chunks {
		if strings.Contains(chunk.RawContent, "rag-asset://") {
			t.Fatalf("internal URL leaked into raw/model text: %+v", chunk)
		}
		if len(chunk.AssetRefs) > 0 {
			imageIndexes = append(imageIndexes, i)
			if chunk.Kind != BlockImage || len(chunk.AssetBindings) != 1 ||
				chunk.AssetBindings[0].OccurrenceID != "occ_1" || chunk.PageNum != 3 {
				t.Fatalf("invalid local image binding: %+v", chunk)
			}
			for _, want := range []string{"before short", "after short", "图片说明：系统架构图", "图片文字：Gateway"} {
				if !strings.Contains(chunk.RawContent, want) {
					t.Fatalf("image-local context missing %q: %q", want, chunk.RawContent)
				}
			}
		}
	}
	if !reflect.DeepEqual(imageIndexes, []int{0}) {
		t.Fatalf("asset association spread beyond local image chunk: indexes=%v chunks=%+v", imageIndexes, chunks)
	}
	for _, chunk := range chunks[1:] {
		if len(chunk.AssetRefs) != 0 {
			t.Fatalf("unrelated chunk inherited image: %+v", chunk)
		}
	}
}

func TestSameAssetMultipleOccurrencesPreserveASTOrder(t *testing.T) {
	t.Parallel()
	location := document.SourceLocation{Kind: document.LocationDocument}
	artifact := imageArtifact(location,
		"![second order value](rag-asset://occ_b)\n\n---\n\n![first order value](rag-asset://occ_a)",
		[]document.ArtifactOccurrence{
			{ID: "occ_a", AssetID: "ast_one", UnitID: "unit_1", Order: 1, Location: location, Caption: "A"},
			{ID: "occ_b", AssetID: "ast_one", UnitID: "unit_1", Order: 9, Location: location, Caption: "B"},
		})
	first := Split(artifact, Config{ChunkSize: 40})
	second := Split(artifact, Config{ChunkSize: 40})
	flatten := func(chunks []Chunk) []string {
		var ids []string
		for _, chunk := range chunks {
			for _, binding := range chunk.AssetBindings {
				ids = append(ids, binding.OccurrenceID)
			}
		}
		return ids
	}
	if got := flatten(first); !reflect.DeepEqual(got, []string{"occ_b", "occ_a"}) {
		t.Fatalf("occurrence association ignored AST reading order: %v", got)
	}
	if !reflect.DeepEqual(flatten(first), flatten(second)) {
		t.Fatal("occurrence order is not deterministic")
	}
	for _, chunk := range first {
		if len(chunk.AssetRefs) != 1 || chunk.AssetRefs[0].ID != "ast_one" {
			t.Fatalf("same canonical asset did not retain distinct occurrences: %+v", chunk)
		}
	}
}

func TestDecorativeImageDoesNotCreateAssetRef(t *testing.T) {
	t.Parallel()
	location := document.SourceLocation{Kind: document.LocationDocument}
	artifact := imageArtifact(location, "before ![decorative](rag-asset://occ_d) after", []document.ArtifactOccurrence{{
		ID: "occ_d", AssetID: "ast_one", UnitID: "unit_1", Location: location,
		Caption: "decoration", Decorative: true,
	}})
	chunks := Split(artifact, Config{ChunkSize: 50})
	if len(chunks) != 1 || len(chunks[0].AssetRefs) != 0 || strings.Contains(chunks[0].RawContent, "decorative") {
		t.Fatalf("decorative image leaked as a retrievable resource: %+v", chunks)
	}
}

func TestUnavailableImageKeepsDescriptionWithoutAssetRef(t *testing.T) {
	t.Parallel()
	location := document.SourceLocation{Kind: document.LocationPage, Index: 2}
	artifact := imageArtifact(location, "![diagram](rag-asset://occ_u)", []document.ArtifactOccurrence{{
		ID: "occ_u", AssetID: "ast_one", UnitID: "unit_1", Location: location,
		Caption: "legacy vector diagram", OCRText: "node A to node B",
	}})
	artifact.Assets[0].DisplayStatus = document.DisplayUnavailable

	chunks := Split(artifact, Config{ChunkSize: 50})
	if len(chunks) != 1 || len(chunks[0].AssetRefs) != 0 || len(chunks[0].AssetBindings) != 0 {
		t.Fatalf("unavailable image became an authorized resource: %+v", chunks)
	}
	if !strings.Contains(chunks[0].RawContent, "legacy vector diagram") ||
		!strings.Contains(chunks[0].RawContent, "node A to node B") {
		t.Fatalf("unavailable image description was lost: %+v", chunks)
	}
}

func TestImageInTableRemainsRowLocal(t *testing.T) {
	t.Parallel()
	location := document.SourceLocation{Kind: document.LocationPage, Index: 1}
	artifact := imageArtifact(location,
		"| item | detail |\n| --- | --- |\n| plain | "+strings.Repeat("x", 70)+" |\n| diagram | ![d](rag-asset://occ_1) |",
		[]document.ArtifactOccurrence{{
			ID: "occ_1", AssetID: "ast_one", UnitID: "unit_1", Order: 2, Location: location,
			Caption: "diagram", OCRText: "node A node B",
		}})
	chunks := Split(artifact, Config{ChunkSize: 35})
	var refChunks int
	for _, chunk := range chunks {
		if len(chunk.AssetRefs) > 0 {
			refChunks++
			if !strings.Contains(chunk.RawContent, "图片说明：diagram") {
				t.Fatalf("table occurrence caption missing: %+v", chunk)
			}
		}
	}
	if refChunks != 1 {
		t.Fatalf("table occurrence spread to repeated unrelated rows: refChunks=%d chunks=%+v", refChunks, chunks)
	}
}

func TestImagesInLongListRemainOccurrenceLocal(t *testing.T) {
	t.Parallel()
	location := document.SourceLocation{Kind: document.LocationDocument}
	markdown := "- ![a](rag-asset://occ_a) first image context\n" +
		strings.Repeat("- unrelated list item with enough text to force another chunk\n", 18) +
		"- ![b](rag-asset://occ_b) second image context"
	artifact := document.ParsedArtifact{
		Units: []document.MarkdownUnit{{ID: "unit_1", Location: location, Markdown: markdown}},
		Assets: []document.ArtifactAsset{
			{ID: "ast_a", Kind: document.AssetKindImage, SourceMIME: "image/png", Width: 10, Height: 10, DisplayStatus: document.DisplayReady},
			{ID: "ast_b", Kind: document.AssetKindImage, SourceMIME: "image/png", Width: 10, Height: 10, DisplayStatus: document.DisplayReady},
		},
		Occurrences: []document.ArtifactOccurrence{
			{ID: "occ_a", AssetID: "ast_a", UnitID: "unit_1", Order: 1, Location: location, Caption: "first-local-image"},
			{ID: "occ_b", AssetID: "ast_b", UnitID: "unit_1", Order: 2, Location: location, Caption: "second-local-image"},
		},
	}
	chunks := Split(artifact, Config{ChunkSize: 34, ChunkOverlap: 3})
	counts := map[string]int{}
	for _, chunk := range chunks {
		for _, binding := range chunk.AssetBindings {
			counts[binding.OccurrenceID]++
			if binding.OccurrenceID == "occ_a" && !strings.Contains(chunk.RawContent, "first-local-image") {
				t.Fatalf("first occurrence escaped its local fragment: %+v", chunk)
			}
			if binding.OccurrenceID == "occ_b" && !strings.Contains(chunk.RawContent, "second-local-image") {
				t.Fatalf("second occurrence escaped its local fragment: %+v", chunk)
			}
		}
	}
	if counts["occ_a"] != 1 || counts["occ_b"] != 1 {
		t.Fatalf("occurrence bindings were duplicated across list chunks: counts=%v chunks=%+v", counts, chunks)
	}
}

func TestResplitChunkKeepsBindingOnOneLocalFragment(t *testing.T) {
	t.Parallel()
	binding := AssetBinding{
		OccurrenceID: "occ_local",
		Asset:        document.AssetRef{ID: "ast_local", Kind: document.AssetKindImage, Caption: "local-diagram"},
		OCRText:      "node alpha to node beta",
	}
	raw := "图片说明：local-diagram\n\n> 图片文字：node alpha to node beta\n\n" + strings.Repeat("unrelated trailing text. ", 30)
	parts := ResplitChunk(Chunk{
		Kind: BlockImage, RawContent: raw, Content: raw, SearchContent: raw,
		AssetBindings: []AssetBinding{binding}, AssetRefs: []AssetRef{binding.Asset},
	}, Config{ChunkSize: 28})
	if len(parts) < 2 {
		t.Fatalf("fixture did not resplit: %+v", parts)
	}
	count := 0
	for _, part := range parts {
		if len(part.AssetBindings) == 0 {
			continue
		}
		count += len(part.AssetBindings)
		if !strings.Contains(part.RawContent, "local-diagram") && !strings.Contains(part.RawContent, "node alpha") {
			t.Fatalf("binding attached to unrelated resplit fragment: %+v", part)
		}
	}
	if count != 1 {
		t.Fatalf("binding appeared %d times after resplit: %+v", count, parts)
	}
}

func imageArtifact(location document.SourceLocation, markdown string, occurrences []document.ArtifactOccurrence) document.ParsedArtifact {
	return document.ParsedArtifact{
		Units: []document.MarkdownUnit{{ID: "unit_1", Location: location, Markdown: markdown}},
		Assets: []document.ArtifactAsset{{
			ID: "ast_one", Kind: document.AssetKindImage, SourceMIME: "image/png",
			Width: 800, Height: 600, DisplayStatus: document.DisplayReady,
		}},
		Occurrences: occurrences,
	}
}
