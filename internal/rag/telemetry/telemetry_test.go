package telemetry

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestEmitDropsUnknownEventsAndSanitizesDimensions(t *testing.T) {
	var events []Event
	recorder := RecorderFunc(func(_ context.Context, event Event) { events = append(events, event) })
	Emit(context.Background(), recorder, EventName("rag.arbitrary"), Fields{DocID: "doc-safe"})
	Emit(context.Background(), recorder, EventDocumentAICall, Fields{
		DocID: "doc-safe", Operation: "vision_page", Outcome: "ok",
		ErrorCode: "provider_error\nsecret=sk-live", TaskID: -1, Duration: -time.Second,
		InputTokens: -4, OutputTokens: 7,
	})
	if len(events) != 1 {
		t.Fatalf("events=%d want=1", len(events))
	}
	got := events[0].Fields
	if got.DocID != "doc-safe" || got.Operation != "vision_page" || got.Outcome != "ok" {
		t.Fatalf("safe fields changed: %+v", got)
	}
	if got.ErrorCode != "" || got.TaskID != 0 || got.Duration != 0 || got.InputTokens != 0 || got.OutputTokens != 7 {
		t.Fatalf("unsafe/negative fields not sanitized: %+v", got)
	}
}

func TestSlogRecorderCannotLogBodiesKeysBase64OrSecrets(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	recorder := NewSlogRecorder(logger)
	canaries := []string{
		"sk-super-secret",
		base64.RawStdEncoding.EncodeToString([]byte("BEGIN PRIVATE DOCUMENT BODY")),
		`E:\\private\\document.pdf`,
		"rag/user/kb/doc/assets/source.png",
		"object_key",
	}
	for _, canary := range canaries {
		Emit(context.Background(), recorder, EventParserDocument, Fields{
			DocID: canary, Format: canary, ParseMode: canary, ParserVersion: canary,
			Operation: canary, Transition: canary, Outcome: canary, ErrorCode: canary,
			CacheKind: canary, CacheStatus: canary,
		})
	}
	Emit(context.Background(), recorder, EventParserDocument, Fields{
		DocID: "doc_e2e", Format: "pdf", ParseMode: "auto",
		ParserVersion: "office-parser-v1+office-wrapper-v2",
		Outcome:       "ok", PageCount: 2, AssetCount: 1, WarningCount: 1,
	})
	text := output.String()
	forbiddenValues := append([]string{
		"BEGIN PRIVATE DOCUMENT", "data:image/png;base64", "api_key",
	}, canaries...)
	for _, forbidden := range forbiddenValues {
		if strings.Contains(text, forbidden) {
			t.Fatalf("structured event leaked %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{
		`"event":"rag.parser.document"`, `"doc_id":"doc_e2e"`, `"asset_count":1`,
		`"parser_version":"office-parser-v1+office-wrapper-v2"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing %s in %s", required, text)
		}
	}
}

func TestDefaultSlogLevelIncludesReleaseGateAggregates(t *testing.T) {
	var output bytes.Buffer
	// A JSON handler without a Level option has slog's production-default Info
	// threshold. The release gate must not depend on enabling debug logs.
	recorder := NewSlogRecorder(slog.New(slog.NewJSONHandler(&output, nil)))
	Emit(context.Background(), recorder, EventParserPages, Fields{
		DocID: "doc_release_gate", Format: "pdf", ParseMode: "auto",
		ParserVersion: "pdf-auto-routing-v1", Outcome: "ok", PageCount: 7, NativePages: 3, VLMPages: 4,
	})
	Emit(context.Background(), recorder, EventResultCache, Fields{
		DocID: "doc_release_gate", CacheKind: "vision_page", CacheStatus: "hit", Outcome: "ok",
	})
	Emit(context.Background(), recorder, EventIndexTask, Fields{
		DocID: "doc_release_gate", Transition: "claim", Outcome: "ok", TaskID: 9,
	})
	text := output.String()
	for _, required := range []string{
		`"event":"rag.parser.pages"`, `"vlm_pages":4`,
		`"event":"rag.result_cache"`, `"cache_status":"hit"`,
		`"event":"rag.index_task"`, `"transition":"claim"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("default slog output missing %s: %s", required, text)
		}
	}
}
