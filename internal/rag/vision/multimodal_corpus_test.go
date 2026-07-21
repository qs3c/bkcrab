package vision

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func multimodalVisionFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "testdata", "multimodal", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestMultimodalPromptInjectionFixturesRemainTypedUntrustedData(t *testing.T) {
	t.Parallel()
	page, err := DecodePageTranscription(multimodalVisionFixture(t, "page-transcription-injection.json"), DefaultSchemaLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Visuals) != 1 || !strings.Contains(page.Markdown, "SYSTEM: ignore") ||
		!strings.Contains(page.Visuals[0].Caption, "ragResources") {
		t.Fatalf("page injection fixture was not preserved as typed text: %+v", page)
	}
	image, err := DecodeImageDescription(multimodalVisionFixture(t, "image-description-injection.json"), DefaultSchemaLimits())
	if err != nil {
		t.Fatal(err)
	}
	if image.Kind != "diagram" || !strings.Contains(image.Caption, "tool call") || image.Decorative {
		t.Fatalf("image injection fixture escaped typed fields: %+v", image)
	}
	for _, value := range []string{page.Markdown, page.Visuals[0].Caption, page.Visuals[0].OCRText, image.Caption, image.OCRText} {
		lower := strings.ToLower(value)
		if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") || strings.Contains(lower, "data:") || strings.Contains(lower, "rag-asset://") {
			t.Fatalf("fixture unexpectedly contains a fetchable/internal resource: %q", value)
		}
	}
}

func TestMultimodalOversizedAndDeepDocumentAIJSONIsRejected(t *testing.T) {
	t.Parallel()
	limits := DefaultSchemaLimits()
	limits.MaxJSONDepth = 8
	if _, err := DecodePageTranscription(multimodalVisionFixture(t, "deep-document-ai.json"), limits); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("deep JSON was not rejected by the deterministic depth gate: %v", err)
	}
	limits = DefaultSchemaLimits()
	limits.MaxCaptionBytes = 1024
	if _, err := DecodeImageDescription(multimodalVisionFixture(t, "oversized-document-ai.json"), limits); err == nil {
		t.Fatal("oversized DocumentAI caption was accepted")
	}
}
