// Package telemetry defines the privacy-safe observability boundary for the
// document RAG pipeline. Callers can only report bounded counters and a small
// set of token-like dimensions; document text, image bytes, object keys,
// endpoints, credentials, and provider response bodies have no representation
// in Event.
package telemetry

import (
	"context"
	"encoding/base64"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

// EventName is deliberately closed. Unknown event names are dropped at the
// boundary instead of becoming an accidental arbitrary structured-log API.
type EventName string

const (
	EventParserDocument      EventName = "rag.parser.document"
	EventParserPages         EventName = "rag.parser.pages"
	EventParserSidecarCall   EventName = "rag.parser.sidecar_call"
	EventResultCache         EventName = "rag.result_cache"
	EventDocumentAIBudget    EventName = "rag.document_ai.budget"
	EventDocumentAICall      EventName = "rag.document_ai.call"
	EventEnrichmentBatch     EventName = "rag.enrichment.batch"
	EventIndexTask           EventName = "rag.index_task"
	EventActiveVersionSwitch EventName = "rag.active_version_switch"
	EventLifecycleGC         EventName = "rag.lifecycle.gc"
)

var allowedEvents = map[EventName]struct{}{
	EventParserDocument: {}, EventParserPages: {}, EventParserSidecarCall: {},
	EventResultCache: {}, EventDocumentAIBudget: {}, EventDocumentAICall: {},
	EventEnrichmentBatch: {}, EventIndexTask: {}, EventActiveVersionSwitch: {}, EventLifecycleGC: {},
}

// Fields is intentionally made only of privacy-safe dimensions and numeric
// measurements. Do not add generic maps, error strings, URLs, paths, request
// bodies, response bodies, captions, OCR, Markdown, or object-store keys.
type Fields struct {
	DocID           string
	TaskID          int64
	DocVersion      int64
	PreviousVersion int64
	RetiredVersion  int64
	ClaimGeneration int64

	Format        string
	ParseMode     string
	ParserVersion string
	Operation     string
	Transition    string
	Outcome       string
	ErrorCode     string
	CacheKind     string
	CacheStatus   string

	Attempt       int
	RetryCount    int
	Duration      time.Duration
	PageCount     int
	NativePages   int
	VLMPages      int
	DegradedPages int
	AssetCount    int
	Decorative    int
	WarningCount  int
	SkippedCount  int
	RequestCount  int
	BlockCount    int
	SuccessCount  int

	InputTokens  int64
	OutputTokens int64
	CostMicroUSD int64
	Estimated    bool
}

// Event is the sanitized value delivered to a recorder.
type Event struct {
	Name   EventName
	At     time.Time
	Fields Fields
}

// Recorder is deliberately tiny so deployments can fan events out to slog,
// counters/histograms, or a test recorder without coupling RAG to one metrics
// implementation.
type Recorder interface {
	Record(context.Context, Event)
}

type RecorderFunc func(context.Context, Event)

func (f RecorderFunc) Record(ctx context.Context, event Event) {
	if f != nil {
		f(ctx, event)
	}
}

type nopRecorder struct{}

func (nopRecorder) Record(context.Context, Event) {}

// NopRecorder returns an allocation-free recorder for explicitly disabled
// telemetry.
func NopRecorder() Recorder { return nopRecorder{} }

// Emit sanitizes an event before handing it to an injected recorder. Invalid
// string dimensions are omitted; an unknown event name drops the whole event.
func Emit(ctx context.Context, recorder Recorder, name EventName, fields Fields) {
	if recorder == nil {
		return
	}
	if _, ok := allowedEvents[name]; !ok {
		return
	}
	recorder.Record(ctx, Event{Name: name, At: time.Now().UTC(), Fields: sanitize(fields)})
}

var (
	// Stable IDs and parser/model contract versions are not small enums, but
	// they are still limited to one bounded token. A plus is required by the
	// checked Office parser contract (for example
	// office-parser-v1+office-wrapper-v3).
	safeToken   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:+-]{0,127}$`)
	secretToken = regexp.MustCompile(`(?i)(^|[._:+-])(secret|api[_-]?key|object[_-]?key|private[_-]?key|access[_-]?token|authorization|bearer)([._:+-]|$)`)
)

type enumSet map[string]struct{}

var (
	allowedFormats = enumSet{
		"md": {}, "markdown": {}, "txt": {}, "pdf": {}, "docx": {}, "pptx": {}, "xlsx": {},
	}
	allowedParseModes = enumSet{"standard": {}, "auto": {}}
	allowedOperations = enumSet{
		"vision_page": {}, "vision_page_repair": {}, "vision_image": {}, "vision_image_repair": {},
		"enrichment": {}, "office-convert": {}, "pdf-analyze": {}, "pdf-render": {}, "usage_reconcile": {},
	}
	allowedTransitions = enumSet{
		"enqueue": {}, "reindex": {}, "claim": {}, "supersede": {}, "heartbeat": {}, "retry": {}, "finish": {}, "activate": {},
		"reserve": {}, "sent": {}, "release": {}, "commit": {}, "commit_overrun": {}, "reconcile": {},
	}
	allowedOutcomes   = enumSet{"ok": {}, "error": {}, "rejected": {}, "scheduled": {}, "skipped": {}}
	allowedErrorCodes = enumSet{
		"store_error": {}, "fence_lost": {}, "maintenance_fence_lost": {}, "maintenance_error": {},
		"maintenance_busy": {}, "vector_error": {}, "permanent_failure": {}, "pending_limit": {},
		"reindex_rate_limit": {}, "cache_io": {}, "canceled": {}, "timeout": {},
		"limit_exceeded": {}, "source_integrity": {}, "capability_unavailable": {}, "invalid_bundle": {},
		"empty_content": {}, "invalid_document": {}, "parser_error": {}, "rate_limit": {}, "upstream": {},
		"http_rejected": {}, "sidecar_error": {}, "invalid_response": {}, "policy": {}, "budget": {},
		"budget_required": {}, "attempt_not_sent": {}, "committed_cache": {}, "internal": {},
		"quota_exceeded": {}, "invalid_fence": {}, "usage_conflict": {}, "ledger_error": {},
		"task_budget_init": {}, "usage_lookup": {}, "invalid_usage_state": {}, "invalid_usage": {},
	}
	allowedCacheKinds    = enumSet{"parse_artifact": {}, "enrichment": {}, "vision_page": {}, "vision_image": {}}
	allowedCacheStatuses = enumSet{"hit": {}, "miss": {}, "stale": {}, "error": {}}
)

func sanitize(fields Fields) Fields {
	fields.DocID = sanitizeStableToken(fields.DocID)
	fields.Format = sanitizeEnum(fields.Format, allowedFormats)
	fields.ParseMode = sanitizeEnum(fields.ParseMode, allowedParseModes)
	fields.ParserVersion = sanitizeStableToken(fields.ParserVersion)
	fields.Operation = sanitizeEnum(fields.Operation, allowedOperations)
	fields.Transition = sanitizeEnum(fields.Transition, allowedTransitions)
	fields.Outcome = sanitizeEnum(fields.Outcome, allowedOutcomes)
	fields.ErrorCode = sanitizeEnum(fields.ErrorCode, allowedErrorCodes)
	fields.CacheKind = sanitizeEnum(fields.CacheKind, allowedCacheKinds)
	fields.CacheStatus = sanitizeEnum(fields.CacheStatus, allowedCacheStatuses)

	fields.TaskID = nonNegative64(fields.TaskID)
	fields.DocVersion = nonNegative64(fields.DocVersion)
	fields.PreviousVersion = nonNegative64(fields.PreviousVersion)
	fields.RetiredVersion = nonNegative64(fields.RetiredVersion)
	fields.ClaimGeneration = nonNegative64(fields.ClaimGeneration)
	fields.Attempt = nonNegative(fields.Attempt)
	fields.RetryCount = nonNegative(fields.RetryCount)
	if fields.Duration < 0 {
		fields.Duration = 0
	}
	fields.PageCount = nonNegative(fields.PageCount)
	fields.NativePages = nonNegative(fields.NativePages)
	fields.VLMPages = nonNegative(fields.VLMPages)
	fields.DegradedPages = nonNegative(fields.DegradedPages)
	fields.AssetCount = nonNegative(fields.AssetCount)
	fields.Decorative = nonNegative(fields.Decorative)
	fields.WarningCount = nonNegative(fields.WarningCount)
	fields.SkippedCount = nonNegative(fields.SkippedCount)
	fields.RequestCount = nonNegative(fields.RequestCount)
	fields.BlockCount = nonNegative(fields.BlockCount)
	fields.SuccessCount = nonNegative(fields.SuccessCount)
	fields.InputTokens = nonNegative64(fields.InputTokens)
	fields.OutputTokens = nonNegative64(fields.OutputTokens)
	fields.CostMicroUSD = nonNegative64(fields.CostMicroUSD)
	return fields
}

func sanitizeStableToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !safeToken.MatchString(value) || containsSecretToken(value) || looksLikeEncodedPayload(value) {
		return ""
	}
	return value
}

func sanitizeEnum(value string, allowed enumSet) string {
	value = strings.TrimSpace(value)
	if _, ok := allowed[value]; !ok {
		return ""
	}
	return value
}

func containsSecretToken(value string) bool {
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "pk-") ||
		strings.HasPrefix(lower, "ghp_") || strings.HasPrefix(lower, "xoxb-") {
		return true
	}
	return secretToken.MatchString(value)
}

// looksLikeEncodedPayload rejects body-shaped base64 without treating every
// long opaque identifier as encoded content. Text payloads and common binary
// document/image signatures are sufficient to catch the accidental logging
// paths this boundary is designed to prevent.
func looksLikeEncodedPayload(value string) bool {
	if len(value) < 20 {
		return false
	}
	encodings := []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding,
	}
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(value)
		if err != nil || len(decoded) < 12 {
			continue
		}
		if encodedBinaryMagic(decoded) || mostlyPrintable(decoded) {
			return true
		}
	}
	return false
}

func encodedBinaryMagic(value []byte) bool {
	for _, magic := range []string{"\x89PNG\r\n\x1a\n", "\xff\xd8\xff", "%PDF-", "PK\x03\x04"} {
		if strings.HasPrefix(string(value), magic) {
			return true
		}
	}
	return false
}

func mostlyPrintable(value []byte) bool {
	printable := 0
	for _, current := range value {
		if current == '\n' || current == '\r' || current == '\t' || current >= 0x20 && current <= 0x7e {
			printable++
		}
	}
	return printable*10 >= len(value)*8
}

func nonNegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func nonNegative64(value int64) int64 {
	if value < 0 {
		return 0
	}
	return value
}

// SlogRecorder emits a stable set of structured attributes. It never accepts
// arbitrary attributes, so a caller cannot accidentally attach a body, image,
// object key, endpoint, or credential.
type SlogRecorder struct {
	Logger *slog.Logger
}

func NewSlogRecorder(logger *slog.Logger) Recorder {
	return &SlogRecorder{Logger: logger}
}

func (r *SlogRecorder) Record(ctx context.Context, event Event) {
	if r == nil {
		return
	}
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}
	attrs := eventAttrs(event.Fields)
	attrs = append([]slog.Attr{slog.String("event", string(event.Name))}, attrs...)
	logger.LogAttrs(ctx, eventLevel(event), "rag telemetry", attrs...)
}

func eventLevel(event Event) slog.Level {
	if event.Fields.Outcome == "error" || event.Fields.Outcome == "rejected" {
		return slog.LevelWarn
	}
	switch event.Name {
	case EventParserDocument, EventParserPages, EventParserSidecarCall, EventResultCache,
		EventDocumentAIBudget, EventDocumentAICall, EventEnrichmentBatch, EventActiveVersionSwitch:
		// Cost and durable quota transitions are release-gate signals. They are
		// visible at the default info level, as are the bounded parser/cache
		// aggregates needed to interpret those costs. Payload-bearing data is
		// impossible to attach through the closed event schema.
		return slog.LevelInfo
	case EventIndexTask, EventLifecycleGC:
		// Successful heartbeats are the one high-frequency event. Claim, retry,
		// finish and activation-related state remain visible by default, while
		// heartbeat failures are promoted to warn above.
		if event.Fields.Transition == "heartbeat" && event.Fields.Outcome == "ok" {
			return slog.LevelDebug
		}
		return slog.LevelInfo
	}
	// These are operational measurements rather than user-facing activity
	// logs. Debug is the safe default; a metrics recorder can consume them at
	// full fidelity without increasing normal log volume.
	return slog.LevelDebug
}

func eventAttrs(fields Fields) []slog.Attr {
	attrs := make([]slog.Attr, 0, 30)
	addString := func(key, value string) {
		if value != "" {
			attrs = append(attrs, slog.String(key, value))
		}
	}
	addInt := func(key string, value int) {
		if value != 0 {
			attrs = append(attrs, slog.Int(key, value))
		}
	}
	addInt64 := func(key string, value int64) {
		if value != 0 {
			attrs = append(attrs, slog.Int64(key, value))
		}
	}
	addString("doc_id", fields.DocID)
	addInt64("task_id", fields.TaskID)
	addInt64("doc_version", fields.DocVersion)
	addInt64("previous_version", fields.PreviousVersion)
	addInt64("retired_version", fields.RetiredVersion)
	addInt64("claim_generation", fields.ClaimGeneration)
	addString("format", fields.Format)
	addString("parse_mode", fields.ParseMode)
	addString("parser_version", fields.ParserVersion)
	addString("operation", fields.Operation)
	addString("transition", fields.Transition)
	addString("outcome", fields.Outcome)
	addString("error_code", fields.ErrorCode)
	addString("cache_kind", fields.CacheKind)
	addString("cache_status", fields.CacheStatus)
	addInt("attempt", fields.Attempt)
	addInt("retry_count", fields.RetryCount)
	if fields.Duration != 0 {
		attrs = append(attrs, slog.Int64("duration_ms", fields.Duration.Milliseconds()))
	}
	addInt("page_count", fields.PageCount)
	addInt("native_pages", fields.NativePages)
	addInt("vlm_pages", fields.VLMPages)
	addInt("degraded_pages", fields.DegradedPages)
	addInt("asset_count", fields.AssetCount)
	addInt("decorative_count", fields.Decorative)
	addInt("warning_count", fields.WarningCount)
	addInt("skipped_count", fields.SkippedCount)
	addInt("request_count", fields.RequestCount)
	addInt("block_count", fields.BlockCount)
	addInt("success_count", fields.SuccessCount)
	addInt64("input_tokens", fields.InputTokens)
	addInt64("output_tokens", fields.OutputTokens)
	addInt64("cost_microusd", fields.CostMicroUSD)
	if fields.Estimated {
		attrs = append(attrs, slog.Bool("usage_estimated", true))
	}
	return attrs
}
