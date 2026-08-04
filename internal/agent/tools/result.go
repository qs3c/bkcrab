package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

const (
	// RAGResourcesMetadataKey is the sole typed metadata key accepted from the
	// trusted rag_search builtin. Tool output text is never inspected for it.
	RAGResourcesMetadataKey   = "ragResources"
	ImageArtifactsMetadataKey = "imageArtifacts"

	// These are transport-boundary limits, not the smaller gallery display
	// limit. A search may return resources from several final hits; the agent
	// loop applies its own display/deduplication cap when it aggregates them.
	maxRAGResourcesPerToolResult   = 64
	maxImageArtifactsPerToolResult = 16
	maxResultMetadataValueBytes    = 256 << 10
	maxResultMetadataLogBytes      = 48
)

// ResultMetadata is an in-process, producer-authenticated side channel for a
// tool result. Values stay as raw JSON until a producer-specific validator has
// checked their schema. They must never be encoded into ToolResult.Text.
type ResultMetadata map[string]json.RawMessage

// Clone returns a detached copy suitable for crossing another in-process
// boundary. In particular, callers cannot mutate a handler-owned RawMessage
// after the registry has validated it.
func (m ResultMetadata) Clone() ResultMetadata {
	if len(m) == 0 {
		return nil
	}
	out := make(ResultMetadata, len(m))
	for key, value := range m {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}

// ToolResult is the typed result of a tool invocation. Text is the only part
// shown to the model; Metadata is an in-memory side channel consumed by the
// current agent turn.
type ToolResult struct {
	Text     string
	Metadata ResultMetadata
}

// ResultHandler is the typed counterpart of ToolFunc.
type ResultHandler func(context.Context, json.RawMessage) (ToolResult, error)

func resultHandlerFromToolFunc(fn ToolFunc) ResultHandler {
	if fn == nil {
		return nil
	}
	return func(ctx context.Context, args json.RawMessage) (ToolResult, error) {
		text, err := fn(ctx, args)
		return ToolResult{Text: text}, err
	}
}

// ragResourceRefWire mirrors rag.RAGResourceRef without importing the parent
// rag package. The rag package implements the agent tool adapter and imports
// this package, so importing it here would create a cycle. The document DTOs
// are lower-level and safe to share.
type ragResourceRefWire struct {
	Asset          document.AssetRef       `json:"asset"`
	KBID           string                  `json:"kbId"`
	KBName         string                  `json:"kbName"`
	DocID          string                  `json:"docId"`
	DocName        string                  `json:"docName"`
	ChunkIndex     int                     `json:"chunkIndex"`
	SectionTitle   string                  `json:"sectionTitle,omitempty"`
	SourceLocation document.SourceLocation `json:"sourceLocation"`
}

type ImageArtifactRef struct {
	BatchID    string `json:"batch_id"`
	TaskID     string `json:"task_id"`
	ItemIndex  int    `json:"item_index"`
	ChunkIndex int    `json:"chunk_index"`
	Label      string `json:"label"`
	Index      int    `json:"index"`
	Path       string `json:"path"`
	URL        string `json:"url,omitempty"`
	MIMEType   string `json:"mime_type"`
	Size       int64  `json:"size"`
	Width      int    `json:"width"`
	Height     int    `json:"height"`
	SHA256     string `json:"sha256"`
	Origin     struct {
		AgentID   string `json:"agent_id"`
		ProjectID string `json:"project_id,omitempty"`
		SessionID string `json:"session_id,omitempty"`
	} `json:"origin"`
}

var (
	canonicalImageBatchID = regexp.MustCompile(`^imgb_[a-z0-9]{16,64}$`)
	canonicalImageTaskID  = regexp.MustCompile(`^imgt_[a-z0-9]{16,64}$`)
	canonicalImageHash    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

func validateResultMetadata(toolName string, source ToolSource, metadata ResultMetadata) ResultMetadata {
	if len(metadata) == 0 {
		return nil
	}

	allowedKey := ""
	if source == SourceBuiltin {
		switch toolName {
		case "rag_search":
			allowedKey = RAGResourcesMetadataKey
		case "image_gen_batch":
			allowedKey = ImageArtifactsMetadataKey
		}
	}
	if allowedKey == "" {
		warnResultMetadataRejected(toolName, source, "producer is not allowed", "", 0)
		return nil
	}

	var out ResultMetadata
	warned := false
	for key, raw := range metadata {
		if key != allowedKey {
			if !warned {
				warnResultMetadataRejected(toolName, source, "metadata key is not allowed", key, len(raw))
				warned = true
			}
			continue
		}
		var err error
		if allowedKey == RAGResourcesMetadataKey {
			err = validateRAGResourcesMetadata(raw)
		} else {
			err = validateImageArtifactsMetadata(raw)
		}
		if err != nil {
			if !warned {
				warnResultMetadataRejected(toolName, source, err.Error(), key, len(raw))
				warned = true
			}
			continue
		}
		if out == nil {
			out = make(ResultMetadata, 1)
		}
		out[key] = append(json.RawMessage(nil), raw...)
	}
	return out
}

func validateImageArtifactsMetadata(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > maxResultMetadataValueBytes {
		return fmt.Errorf("metadata value is empty or exceeds byte limit")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var refs []ImageArtifactRef
	if err := dec.Decode(&refs); err != nil || refs == nil || ensureJSONEOF(dec) != nil {
		return fmt.Errorf("metadata value has invalid shape")
	}
	if len(refs) > maxImageArtifactsPerToolResult {
		return fmt.Errorf("metadata value exceeds artifact count limit")
	}
	var previous *ImageArtifactRef
	seen := make(map[string]struct{}, len(refs))
	for i := range refs {
		ref := &refs[i]
		if err := validateImageArtifactRef(*ref); err != nil {
			return fmt.Errorf("metadata artifact %d has invalid shape", i)
		}
		key := ref.BatchID + "\x00" + ref.TaskID + "\x00" + ref.Path
		if _, ok := seen[key]; ok {
			return fmt.Errorf("metadata artifact %d is duplicated", i)
		}
		seen[key] = struct{}{}
		if previous != nil && !imageArtifactBefore(*previous, *ref) {
			return fmt.Errorf("metadata artifacts are not in stable order")
		}
		previous = ref
	}
	return nil
}

func validateImageArtifactRef(ref ImageArtifactRef) error {
	if !canonicalImageBatchID.MatchString(ref.BatchID) || !canonicalImageTaskID.MatchString(ref.TaskID) || ref.URL != "" ||
		ref.ItemIndex < 0 || ref.ChunkIndex < 0 || ref.Index < 0 || ref.Size < 1 || ref.Width < 1 || ref.Height < 1 ||
		!canonicalImageHash.MatchString(ref.SHA256) || !safeImageArtifactSegment(ref.Origin.AgentID, false) ||
		!safeImageArtifactSegment(ref.Origin.ProjectID, true) || !safeImageArtifactSegment(ref.Origin.SessionID, true) {
		return fmt.Errorf("identity or dimensions are invalid")
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp"}
	extension, ok := ext[ref.MIMEType]
	if !ok {
		return fmt.Errorf("MIME type is invalid")
	}
	clean := path.Clean(ref.Path)
	prefix := path.Join("imagegen", ref.BatchID, ref.TaskID, "claims") + "/"
	if clean != ref.Path || !strings.HasPrefix(clean, prefix) {
		return fmt.Errorf("path is invalid")
	}
	rest := strings.TrimPrefix(clean, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 2 {
		return fmt.Errorf("path is invalid")
	}
	generation, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || generation < 1 {
		return fmt.Errorf("claim generation is invalid")
	}
	wantName := fmt.Sprintf("image-%d-%s%s", ref.Index, ref.SHA256, extension)
	if parts[1] != wantName {
		return fmt.Errorf("artifact filename is invalid")
	}
	return nil
}

func safeImageArtifactSegment(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	return len(value) <= 191 && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func imageArtifactBefore(a, b ImageArtifactRef) bool {
	if a.ItemIndex != b.ItemIndex {
		return a.ItemIndex < b.ItemIndex
	}
	if a.ChunkIndex != b.ChunkIndex {
		return a.ChunkIndex < b.ChunkIndex
	}
	if a.Index != b.Index {
		return a.Index < b.Index
	}
	if a.TaskID != b.TaskID {
		return a.TaskID < b.TaskID
	}
	return a.Path < b.Path
}

func validateRAGResourcesMetadata(raw json.RawMessage) error {
	if len(raw) == 0 {
		return fmt.Errorf("metadata value is empty")
	}
	if len(raw) > maxResultMetadataValueBytes {
		return fmt.Errorf("metadata value exceeds byte limit")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var refs []ragResourceRefWire
	if err := dec.Decode(&refs); err != nil {
		return fmt.Errorf("metadata value has invalid shape")
	}
	if refs == nil {
		return fmt.Errorf("metadata value must be an array")
	}
	if err := ensureJSONEOF(dec); err != nil {
		return fmt.Errorf("metadata value has trailing data")
	}
	if len(refs) > maxRAGResourcesPerToolResult {
		return fmt.Errorf("metadata value exceeds resource count limit")
	}
	for i, ref := range refs {
		if err := validateRAGResourceRef(ref); err != nil {
			return fmt.Errorf("metadata resource %d has invalid shape", i)
		}
	}
	return nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("unexpected extra JSON value")
		}
		return err
	}
	return nil
}

func validateRAGResourceRef(ref ragResourceRefWire) error {
	if !canonicalRAGAssetID(ref.Asset.ID) || ref.Asset.Kind != document.AssetKindImage {
		return fmt.Errorf("asset identity is invalid")
	}
	if strings.TrimSpace(ref.KBID) == "" || strings.TrimSpace(ref.DocID) == "" || ref.ChunkIndex < 0 {
		return fmt.Errorf("resource source identity is invalid")
	}
	if ref.Asset.PageNum < 0 || ref.Asset.Width < 0 || ref.Asset.Height < 0 {
		return fmt.Errorf("asset dimensions are invalid")
	}
	if ref.Asset.MIMEType != "" && !strings.HasPrefix(strings.ToLower(ref.Asset.MIMEType), "image/") {
		return fmt.Errorf("asset MIME type is invalid")
	}
	// Asset.Location can be empty for older hydrated chunks; BuildRAGResourceRefs
	// deliberately falls back to the hit's SourceLocation in that case.
	if ref.Asset.Location.Kind != "" {
		if err := ref.Asset.Location.Validate(); err != nil {
			return err
		}
	}
	return ref.SourceLocation.Validate()
}

func canonicalRAGAssetID(id string) bool {
	const prefix = "ast_"
	if len(id) != len(prefix)+32 || !strings.HasPrefix(id, prefix) {
		return false
	}
	for _, c := range id[len(prefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// The warning deliberately includes only bounded structural facts, never the
// raw metadata or its document-derived captions.
func warnResultMetadataRejected(toolName string, source ToolSource, reason, key string, size int) {
	logTool := boundedResultMetadataLogValue(toolName)
	logKey := key
	if logKey != "" && logKey != RAGResourcesMetadataKey && logKey != ImageArtifactsMetadataKey {
		logKey = "<untrusted>"
	}
	logKey = boundedResultMetadataLogValue(logKey)
	logReason := boundedResultMetadataLogValue(reason)
	slog.Warn("tool result metadata rejected",
		"tool", logTool,
		"source", toolSourceName(source),
		"key", logKey,
		"key_bytes", len(key),
		"bytes", size,
		"reason", logReason,
	)
}

// boundedResultMetadataLogValue keeps attacker-controlled identifiers from
// expanding or forging structured warning lines. It preserves a short useful
// prefix, replaces controls, and never splits a UTF-8 rune.
func boundedResultMetadataLogValue(value string) string {
	if value == "" {
		return ""
	}
	const suffix = "..."
	var out strings.Builder
	truncated := false
	for _, r := range value {
		if unicode.IsControl(r) {
			r = '?'
		}
		runeBytes := utf8.RuneLen(r)
		if runeBytes < 0 {
			r = utf8.RuneError
			runeBytes = utf8.RuneLen(r)
		}
		if out.Len()+runeBytes > maxResultMetadataLogBytes-len(suffix) {
			truncated = true
			break
		}
		out.WriteRune(r)
	}
	if truncated || out.Len() < len(value) {
		out.WriteString(suffix)
	}
	return out.String()
}
