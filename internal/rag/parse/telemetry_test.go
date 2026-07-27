package parse

import (
	"context"
	"image/color"
	"strings"
	"sync"
	"testing"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/parse/sidecar"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
	"github.com/qs3c/bkcrab/internal/rag/vision"
)

type parserTelemetryCollector struct {
	mu     sync.Mutex
	events []telemetry.Event
}

func (c *parserTelemetryCollector) Record(_ context.Context, event telemetry.Event) {
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
}

func (c *parserTelemetryCollector) snapshot() []telemetry.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]telemetry.Event(nil), c.events...)
}

func TestParserTelemetryReportsNativeVLMAssetsDecorativeAndWarningsWithoutContent(t *testing.T) {
	source := fakePDFSource([]byte("%PDF telemetry fixture contains BEGIN PRIVATE DOCUMENT"))
	nativeText := strings.Repeat("native anchor ", 10)
	nativePage := primitiveWithText(nativeText)
	nativePage.Page = 1
	visionPage := primitiveWithText(nativeText)
	visionPage.Page = 2
	visionPage.Signals.Table = true
	extractor := &pdfFixtureExtractor{
		t: t, source: source,
		analyzePages: []analyzePageFixture{
			{status: sidecar.PageStatusOK, native: nativeText, primitive: nativePage},
			{status: sidecar.PageStatusOK, native: nativeText, primitive: visionPage},
		},
		renderPages: map[int]renderPageFixture{
			2: {status: sidecar.PageStatusOK, render: solidPNG(t, 40, 40, color.White)},
		},
	}
	transcriber := &fakePDFVision{results: map[int]vision.PageTranscription{
		2: {Markdown: nativeText},
	}, errors: map[int]error{}}
	collector := &parserTelemetryCollector{}
	parser := NewLocalParser(extractor, 300)
	parser.TempDir = t.TempDir()
	parser.SetRecorder(collector)
	parsed, err := parser.Parse(context.Background(), source, ParseOptions{
		Mode: config.ParseModeAuto, PageTranscriber: transcriber,
		DocumentAIBudget: &vision.TaskDocumentAIBudget{},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer parsed.Close()

	events := collector.snapshot()
	var documentEvent, pageEvent *telemetry.Event
	for index := range events {
		event := &events[index]
		switch event.Name {
		case telemetry.EventParserDocument:
			documentEvent = event
		case telemetry.EventParserPages:
			pageEvent = event
		}
	}
	if documentEvent == nil || documentEvent.Fields.PageCount != 2 || documentEvent.Fields.Outcome != "ok" {
		t.Fatalf("document event=%+v", documentEvent)
	}
	if pageEvent == nil || pageEvent.Fields.PageCount != 2 || pageEvent.Fields.NativePages != 1 ||
		pageEvent.Fields.VLMPages != 1 || pageEvent.Fields.DegradedPages != 0 {
		t.Fatalf("page event=%+v", pageEvent)
	}
	for _, event := range events {
		encoded := event.Fields.DocID + event.Fields.ErrorCode + event.Fields.ParserVersion
		if strings.Contains(encoded, "BEGIN PRIVATE DOCUMENT") || strings.Contains(encoded, "rag/") {
			t.Fatalf("telemetry leaked source content or object key: %+v", event)
		}
	}
}
