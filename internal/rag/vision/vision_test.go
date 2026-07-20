package vision

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/objects"
)

func TestDecodePageTranscriptionStrictSchemaAndMarkers(t *testing.T) {
	valid := []byte(`{
		"markdown":"## 安装\n\n![流程图](rag-visual://v1.2)",
		"visuals":[{"key":"v1.2","kind":"diagram","bbox":[10,20,900,800],"caption":"安装流程图","ocrText":"download -> start","decorative":false,"confidence":0.9}]
	}`)
	got, err := DecodePageTranscription(valid, DefaultSchemaLimits())
	if err != nil {
		t.Fatalf("decode valid page transcription: %v", err)
	}
	if got.Visuals[0].BBox != (document.NormalizedBBox{10, 20, 900, 800}) {
		t.Fatalf("bbox = %v", got.Visuals[0].BBox)
	}

	tests := map[string]string{
		"unknown field":       `{"markdown":"text","visuals":[],"extra":true}`,
		"duplicate visual":    `{"markdown":"![a](rag-visual://v1)","visuals":[{"key":"v1","kind":"diagram","bbox":[0,0,1,1],"caption":"a","ocrText":"","decorative":false,"confidence":1},{"key":"v1","kind":"diagram","bbox":[0,0,1,1],"caption":"a","ocrText":"","decorative":false,"confidence":1}]}`,
		"missing marker":      `{"markdown":"text","visuals":[{"key":"v1","kind":"diagram","bbox":[0,0,1,1],"caption":"a","ocrText":"","decorative":false,"confidence":1}]}`,
		"marker twice":        `{"markdown":"![a](rag-visual://v1) ![b](rag-visual://v1)","visuals":[{"key":"v1","kind":"diagram","bbox":[0,0,1,1],"caption":"a","ocrText":"","decorative":false,"confidence":1}]}`,
		"orphan marker":       `{"markdown":"![a](rag-visual://missing)","visuals":[]}`,
		"invalid bbox":        `{"markdown":"![a](rag-visual://v1)","visuals":[{"key":"v1","kind":"diagram","bbox":[20,20,10,30],"caption":"a","ocrText":"","decorative":false,"confidence":1}]}`,
		"external URL":        `{"markdown":"[secret](https://example.invalid/x)","visuals":[]}`,
		"data URI":            `{"markdown":"![x](data:image/png;base64,AAAA)","visuals":[]}`,
		"asset scheme":        `{"markdown":"![x](rag-asset://ast_other)","visuals":[]}`,
		"ftp URI":             `{"markdown":"[secret](ftp://example.invalid/x)","visuals":[]}`,
		"mailto URI":          `{"markdown":"mailto:ops@example.invalid","visuals":[]}`,
		"object URI":          `{"markdown":"s3://private-bucket/key","visuals":[]}`,
		"protocol relative":   `{"markdown":"![x](//example.invalid/x)","visuals":[]}`,
		"internal relative":   `{"markdown":"//document-service","visuals":[]}`,
		"custom internal URI": `{"markdown":"rag-secret://tenant/object","visuals":[]}`,
		"relative link":       `{"markdown":"[secret](/internal/path)","visuals":[]}`,
		"email autolink":      `{"markdown":"<ops@example.invalid>","visuals":[]}`,
		"marker suffix":       `{"markdown":"![a](rag-visual://v1/extra)","visuals":[{"key":"v1","kind":"diagram","bbox":[0,0,1,1],"caption":"a","ocrText":"","decorative":false,"confidence":1}]}`,
		"marker query":        `{"markdown":"![a](rag-visual://v1?secret=1)","visuals":[{"key":"v1","kind":"diagram","bbox":[0,0,1,1],"caption":"a","ocrText":"","decorative":false,"confidence":1}]}`,
		"plain marker":        `{"markdown":"rag-visual://v1","visuals":[{"key":"v1","kind":"diagram","bbox":[0,0,1,1],"caption":"a","ocrText":"","decorative":false,"confidence":1}]}`,
		"URL in caption":      `{"markdown":"![a](rag-visual://v1)","visuals":[{"key":"v1","kind":"diagram","bbox":[0,0,1,1],"caption":"https://example.invalid","ocrText":"","decorative":false,"confidence":1}]}`,
		"unknown visual kind": `{"markdown":"![a](rag-visual://v1)","visuals":[{"key":"v1","kind":"system_prompt","bbox":[0,0,1,1],"caption":"a","ocrText":"","decorative":false,"confidence":1}]}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodePageTranscription([]byte(raw), DefaultSchemaLimits()); err == nil {
				t.Fatal("expected strict schema rejection")
			}
		})
	}
}

func TestDecodeImageDescriptionStrictSchema(t *testing.T) {
	valid := []byte(`{"kind":"diagram","caption":"请求依次经过网关和检索器","ocrText":"Gateway -> Retriever","decorative":false,"confidence":0.95}`)
	if _, err := DecodeImageDescription(valid, DefaultSchemaLimits()); err != nil {
		t.Fatalf("valid image description: %v", err)
	}
	for name, raw := range map[string]string{
		"unknown":           `{"kind":"diagram","caption":"x","ocrText":"","decorative":false,"confidence":1,"url":"x"}`,
		"kind":              `{"kind":"instruction","caption":"x","ocrText":"","decorative":false,"confidence":1}`,
		"confidence":        `{"kind":"diagram","caption":"x","ocrText":"","decorative":false,"confidence":2}`,
		"url":               `{"kind":"diagram","caption":"https://example.invalid/x","ocrText":"","decorative":false,"confidence":1}`,
		"base64":            `{"kind":"diagram","caption":"data:image/png;base64,AAAA","ocrText":"","decorative":false,"confidence":1}`,
		"scheme":            `{"kind":"diagram","caption":"rag-visual://v1","ocrText":"","decorative":false,"confidence":1}`,
		"ftp":               `{"kind":"diagram","caption":"ftp://example.invalid/x","ocrText":"","decorative":false,"confidence":1}`,
		"mailto":            `{"kind":"diagram","caption":"mailto:ops@example.invalid","ocrText":"","decorative":false,"confidence":1}`,
		"object URI":        `{"kind":"diagram","caption":"s3://private-bucket/key","ocrText":"","decorative":false,"confidence":1}`,
		"relative":          `{"kind":"diagram","caption":"//example.invalid/x","ocrText":"","decorative":false,"confidence":1}`,
		"internal relative": `{"kind":"diagram","caption":"//document-service","ocrText":"","decorative":false,"confidence":1}`,
		"internal":          `{"kind":"diagram","caption":"rag-secret://tenant/object","ocrText":"","decorative":false,"confidence":1}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeImageDescription([]byte(raw), DefaultSchemaLimits()); err == nil {
				t.Fatal("expected strict schema rejection")
			}
		})
	}
	limits := DefaultSchemaLimits()
	limits.MaxCaptionBytes = 4
	if _, err := DecodeImageDescription(valid, limits); err == nil {
		t.Fatal("expected caption byte limit rejection")
	}
}

func TestObjectCacheRoundTripAndCorruptionIsMiss(t *testing.T) {
	ctx := context.Background()
	store := objects.NewLocalFS(t.TempDir())
	cache := NewObjectCache(store, DefaultSchemaLimits())
	scope := CacheScope{UserID: "u_1", KBID: "kb_1", DocID: "doc_1"}
	key := strings.Repeat("a", 64)
	want := PageTranscription{Markdown: "page text"}
	if err := cache.PutPage(ctx, scope, key, want); err != nil {
		t.Fatalf("put page cache: %v", err)
	}
	got, ok, err := cache.GetPage(ctx, scope, key)
	if err != nil || !ok || got.Markdown != want.Markdown {
		t.Fatalf("get page cache: got=%+v ok=%v err=%v", got, ok, err)
	}
	objectKey, err := document.PageCacheObjectKey(scope.UserID, scope.KBID, scope.DocID, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(ctx, objectKey, strings.NewReader(`{"schemaVersion":"wrong"}`), -1, "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.GetPage(ctx, scope, key); err != nil || ok {
		t.Fatalf("corrupt cache should be a miss: ok=%v err=%v", ok, err)
	}
}

func TestVisionObjectCacheAcceptsCompleteValidSchemaEnvelopes(t *testing.T) {
	ctx := context.Background()
	limits := DefaultSchemaLimits()
	limits.MaxMarkdownBytes = 80 << 10
	limits.MaxVisuals = 4
	limits.MaxCaptionBytes = 40 << 10
	limits.MaxOCRBytes = 32 << 10
	limits.MaxDescriptionBytes = 70 << 10
	cache := NewObjectCache(objects.NewLocalFS(t.TempDir()), limits)
	scope := CacheScope{UserID: "u_1", KBID: "kb_1", DocID: "doc_1"}

	var markdown strings.Builder
	visuals := make([]Visual, 0, limits.MaxVisuals)
	for index := 0; index < limits.MaxVisuals; index++ {
		key := fmt.Sprintf("v%d", index)
		fmt.Fprintf(&markdown, "![visual %d](rag-visual://%s)\n", index, key)
		visuals = append(visuals, Visual{
			Key: key, Kind: "diagram", BBox: document.NormalizedBBox{0, 0, 1000, 1000},
			Caption: strings.Repeat("caption text ", 3_000), OCRText: strings.Repeat("recognized text ", 1_800),
			Confidence: 1,
		})
	}
	markdown.WriteString(strings.Repeat("page text ", 5_000))
	page := PageTranscription{Markdown: markdown.String(), Visuals: visuals}
	pageKey := strings.Repeat("c", 64)
	if err := cache.PutPage(ctx, scope, pageKey, page); err != nil {
		t.Fatalf("put maximum-shaped page cache: %v", err)
	}
	gotPage, ok, err := cache.GetPage(ctx, scope, pageKey)
	if err != nil || !ok || len(gotPage.Visuals) != len(page.Visuals) || gotPage.Markdown != page.Markdown {
		t.Fatalf("get maximum-shaped page cache: visuals=%d ok=%v err=%v", len(gotPage.Visuals), ok, err)
	}

	image := ImageDescription{
		Kind: "diagram", Caption: strings.Repeat("caption text ", 3_000), OCRText: strings.Repeat("recognized text ", 1_800), Confidence: 1,
	}
	imageKey := strings.Repeat("d", 64)
	if err := cache.PutImage(ctx, scope, imageKey, image); err != nil {
		t.Fatalf("put maximum-shaped image cache: %v", err)
	}
	gotImage, ok, err := cache.GetImage(ctx, scope, imageKey)
	if err != nil || !ok || gotImage.Caption != image.Caption || gotImage.OCRText != image.OCRText {
		t.Fatalf("get maximum-shaped image cache: ok=%v err=%v", ok, err)
	}
}
