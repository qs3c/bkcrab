package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	RAGParseModeStandard = "standard"
	RAGParseModeAuto     = "auto"

	RAGDocumentVersionPending    = "PENDING"
	RAGDocumentVersionRunning    = "RUNNING"
	RAGDocumentVersionDone       = "DONE"
	RAGDocumentVersionRetired    = "RETIRED"
	RAGDocumentVersionGCED       = "GCED"
	RAGDocumentVersionFailed     = "FAILED"
	RAGDocumentVersionSuperseded = "SUPERSEDED"

	RAGDocumentAIUsageReserved  = "RESERVED"
	RAGDocumentAIUsageSent      = "SENT"
	RAGDocumentAIUsageCommitted = "COMMITTED"
	RAGDocumentAIUsageReleased  = "RELEASED"
	RAGDocumentAIUsageOverrun   = "OVERRUN"

	ragLegacyVersionSentinel = "legacy-v0"
	// Composite chunk refs use three bind parameters each. Keeping batches at
	// 250 stays below conservative SQLite parameter limits on older builds.
	maxRAGBatchRecords = 250
)

var (
	ErrRAGBatchTooLarge               = errors.New("store: RAG batch too large")
	ErrRAGAssetConflict               = errors.New("store: immutable RAG asset conflict")
	ErrRAGDocumentVersionMismatch     = errors.New("store: RAG document/version identity mismatch")
	ErrRAGDocumentVersionConflict     = errors.New("store: RAG document version changed")
	ErrRAGDocumentVersionIncomplete   = errors.New("store: incomplete immutable RAG document version snapshot")
	ErrRAGDocumentSourceConflict      = errors.New("store: RAG document source SHA-256 conflicts with version snapshot")
	ErrRAGAdvancedPendingLimit        = errors.New("store: RAG advanced pending task limit exceeded")
	ErrRAGAdvancedReindexRateLimit    = errors.New("store: RAG advanced reindex interval has not elapsed")
	ErrRAGLegacyTaskMigrationRequired = errors.New("store: legacy RAG task migration requires explicit offline-v1 acknowledgement")
	ErrRAGLegacySnapshotBuilder       = errors.New("store: legacy RAG runnable task requires a snapshot builder")
)

// RAGAdvancedEnqueuePolicy is enforced in the same transaction that creates
// an advanced index task. Zero values mean the caller intentionally bypasses
// the policy (used only by migrations and low-level compatibility helpers).
type RAGAdvancedEnqueuePolicy struct {
	UserID             string
	MaxPendingTasks    int
	MinReindexInterval time.Duration
}

func ragCanonicalSHA256(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || value != strings.ToLower(value) || len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func ragSnapshotString(name, value string, maxRunes int, required bool) error {
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%w: %s is not trimmed", ErrRAGDocumentVersionIncomplete, name)
	}
	if required && value == "" {
		return fmt.Errorf("%w: %s is empty", ErrRAGDocumentVersionIncomplete, name)
	}
	if maxRunes > 0 && utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("%w: %s exceeds %d characters", ErrRAGDocumentVersionIncomplete, name, maxRunes)
	}
	return nil
}

// validateRunnableRAGVersionSnapshot validates the complete, secret-free
// immutable input contract. It deliberately does not recompute fingerprints:
// the store lacks parser/provider inputs such as endpoints, DPI and schema
// versions, so the trusted SnapshotBuilder remains responsible for their
// values while the store enforces canonical shape and conditional presence.
func validateRunnableRAGVersionSnapshot(version *RAGDocumentVersionRecord) error {
	if version == nil {
		return fmt.Errorf("%w: snapshot is nil", ErrRAGDocumentVersionIncomplete)
	}
	if err := ragSnapshotString("doc_id", version.DocID, 120, true); err != nil {
		return err
	}
	if version.DocVersion <= 0 {
		return fmt.Errorf("%w: doc_version must be positive", ErrRAGDocumentVersionIncomplete)
	}
	if !ragCanonicalSHA256(version.SourceSHA256) {
		return fmt.Errorf("%w: source_sha256 is not canonical SHA-256", ErrRAGDocumentVersionIncomplete)
	}
	if version.ParseMode != RAGParseModeStandard && version.ParseMode != RAGParseModeAuto {
		return fmt.Errorf("%w: invalid parse_mode %q", ErrRAGDocumentVersionIncomplete, version.ParseMode)
	}
	if version.ChunkSize <= 0 || version.ChunkOverlap < 0 || version.ChunkOverlap >= version.ChunkSize {
		return fmt.Errorf("%w: invalid chunk contract", ErrRAGDocumentVersionIncomplete)
	}
	for _, field := range []struct {
		name  string
		value string
		max   int
	}{
		{"parser_version", version.ParserVersion, 64},
		{"splitter_version", version.SplitterVersion, 64},
		{"embedding_model", version.EmbeddingModel, 128},
	} {
		if err := ragSnapshotString(field.name, field.value, field.max, true); err != nil {
			return err
		}
	}
	for _, fingerprint := range []struct {
		name  string
		value string
	}{
		{"parse_fingerprint", version.ParseFingerprint},
		{"index_fingerprint", version.IndexFingerprint},
		{"embedding_contract_fingerprint", version.EmbeddingContractFingerprint},
	} {
		if !ragCanonicalSHA256(fingerprint.value) {
			return fmt.Errorf("%w: %s is not canonical SHA-256", ErrRAGDocumentVersionIncomplete, fingerprint.name)
		}
	}
	if version.EmbeddingProvider != "system" && version.EmbeddingProvider != "user" {
		return fmt.Errorf("%w: invalid embedding_provider %q", ErrRAGDocumentVersionIncomplete, version.EmbeddingProvider)
	}
	if version.EmbeddingDimensions <= 0 {
		return fmt.Errorf("%w: embedding_dimensions must be positive", ErrRAGDocumentVersionIncomplete)
	}
	if version.MaxDocumentAIRequests <= 0 || version.MaxDocumentAITokens <= 0 || version.MaxDocumentAICostMicroUSD <= 0 {
		return fmt.Errorf("%w: DocumentAI budget caps must be positive", ErrRAGDocumentVersionIncomplete)
	}

	conditionalStrings := []struct {
		name     string
		value    string
		max      int
		required bool
	}{
		{"vision_model", version.VisionModel, 128, version.ParseMode == RAGParseModeAuto},
		{"vision_prompt_version", version.VisionPromptVersion, 64, version.ParseMode == RAGParseModeAuto},
		{"text_model", version.TextModel, 128, version.EnrichmentEnabled},
		{"enrichment_prompt_version", version.EnrichmentPromptVersion, 64, version.EnrichmentEnabled},
	}
	for _, field := range conditionalStrings {
		if err := ragSnapshotString(field.name, field.value, field.max, field.required); err != nil {
			return err
		}
	}
	for _, fingerprint := range []struct {
		name     string
		value    string
		required bool
	}{
		{"vision_provider_fingerprint", version.VisionProviderFingerprint, version.ParseMode == RAGParseModeAuto},
		{"text_provider_fingerprint", version.TextProviderFingerprint, version.EnrichmentEnabled},
	} {
		if fingerprint.value == "" && !fingerprint.required {
			continue
		}
		if !ragCanonicalSHA256(fingerprint.value) {
			return fmt.Errorf("%w: %s is not canonical SHA-256", ErrRAGDocumentVersionIncomplete, fingerprint.name)
		}
	}
	return nil
}

// RAGKBRecord is one user-owned knowledge base. The embedding provider,
// model, and dimensions are snapshotted when the KB is created and are not
// changed by UpdateRAGKB.
type RAGKBRecord struct {
	ID                string
	UserID            string
	Name              string
	Description       string
	EmbedProvider     string
	EmbedModel        string
	EmbedDims         int
	ChunkSize         int
	ChunkOverlap      int
	ParseMode         string
	EnrichmentEnabled bool
	Status            string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RAGDocumentRecord tracks an uploaded source document, its newest target
// version, and the independently pinned retrieval-visible active version.
type RAGDocumentRecord struct {
	ID                 string
	KBID               string
	FileName           string
	FileType           string
	FileSize           int64
	ObjectKey          string
	Status             string
	ErrorMsg           string
	ChunkCount         int
	TokenCount         int
	Version            int64
	SourceSHA256       string
	ActiveVersion      int64
	IndexFormatVersion int
	ProcessingStage    string
	ProgressCurrent    int
	ProgressTotal      int
	ProgressUnit       string
	Degraded           bool
	WarningCount       int
	UploadedAt         time.Time
	IndexedAt          *time.Time
}

// RAGIndexTaskRecord is the durable recovery record for asynchronous document
// indexing. PENDING and RUNNING rows are both recoverable after a restart.
type RAGIndexTaskRecord struct {
	ID              int64
	DocID           string
	DocVersion      int64
	Status          string
	RetryCount      int
	MaxRetry        int
	ClaimGeneration int64
	LeaseOwner      string
	LeaseUntil      *time.Time
	HeartbeatAt     *time.Time
	NextRunAt       *time.Time
	ErrorMsg        string
	CreatedAt       time.Time
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

// RAGDocumentVersionRecord stores the immutable configuration snapshot and
// fenced result state for one physical document index version. Only the result
// fields represented by RAGDocumentVersionResult may change after creation.
type RAGDocumentVersionRecord struct {
	DocID                        string
	DocVersion                   int64
	Status                       string
	SourceSHA256                 string
	ParseMode                    string
	ChunkSize                    int
	ChunkOverlap                 int
	ParserVersion                string
	SplitterVersion              string
	ParseFingerprint             string
	IndexFingerprint             string
	VisionModel                  string
	VisionProviderFingerprint    string
	VisionPromptVersion          string
	TextModel                    string
	TextProviderFingerprint      string
	EnrichmentPromptVersion      string
	EnrichmentEnabled            bool
	MaxDocumentAIRequests        int
	MaxDocumentAITokens          int64
	MaxDocumentAICostMicroUSD    int64
	EmbeddingProvider            string
	EmbeddingModel               string
	EmbeddingDimensions          int
	EmbeddingContractFingerprint string
	ParseArtifactKey             string
	PageCount                    int
	AssetCount                   int
	Degraded                     bool
	WarningCount                 int
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
}

type RAGDocumentVersionResult struct {
	Status           string
	ParseArtifactKey string
	PageCount        int
	AssetCount       int
	Degraded         bool
	WarningCount     int
}

type RAGChunkRef struct {
	DocID      string
	DocVersion int64
	ChunkIndex int
}

type RAGChunkRecord struct {
	KBID          string
	DocID         string
	DocVersion    int64
	ChunkIndex    int
	SectionTitle  string
	LocationJSON  string
	RawContent    string
	Enhancement   string
	SearchContent string
	TokenCount    int
	CreatedAt     time.Time
}

type RAGAssetRecord struct {
	ID                 string
	DocID              string
	ContentSHA256      string
	SourceKind         string
	SourceMIME         string
	DisplayMIME        string
	SourceObjectKey    string
	DisplayObjectKey   string
	ThumbnailObjectKey string
	DisplayStatus      string
	DisplaySHA256      string
	ThumbnailSHA256    string
	ByteSize           int64
	Width              int
	Height             int
	FirstSeenVersion   int64
	LastSeenVersion    int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type RAGChunkAssetRecord struct {
	DocID        string
	DocVersion   int64
	ChunkIndex   int
	AssetID      string
	Ordinal      int
	LocationJSON string
	Caption      string
	OCRText      string
}

type RAGIndexGCTaskRecord struct {
	ID              int64
	DocID           string
	RetiredVersion  int64
	RetiredAt       time.Time
	NotBefore       time.Time
	Status          string
	ClaimGeneration int64
	LeaseOwner      string
	LeaseUntil      *time.Time
	HeartbeatAt     *time.Time
	AttemptCount    int
	NextRunAt       *time.Time
	CreatedAt       time.Time
}

type RAGDocumentAITaskBudgetRecord struct {
	TaskID              int64
	UserID              string
	MaxRequests         int64
	MaxTokens           int64
	MaxCostMicroUSD     int64
	ChargedRequests     int64
	ChargedTokens       int64
	ChargedCostMicroUSD int64
	UpdatedAt           time.Time
}

type RAGDocumentAIUserBudgetRecord struct {
	UserID              string
	PeriodStartUTC      time.Time
	ChargedRequests     int64
	ChargedTokens       int64
	ChargedCostMicroUSD int64
	UpdatedAt           time.Time
}

type RAGDocumentAIUsageRecord struct {
	IdempotencyKey        string
	LogicalRequestKey     string
	UserID                string
	DocID                 string
	TaskID                int64
	DocVersion            int64
	ClaimGeneration       int64
	LeaseOwner            string
	Operation             string
	ProviderFingerprint   string
	PeriodStartUTC        time.Time
	ReservedInputTokens   int64
	ReservedOutputTokens  int64
	ActualInputTokens     int64
	ActualOutputTokens    int64
	EstimatedCostMicroUSD int64
	State                 string
	ReservationExpiresAt  *time.Time
	SentAt                *time.Time
	UsageEstimated        bool
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

// RAGChatTurnRecord is one persisted question/answer exchange in the simple
// knowledge-base chat UI. Sources is the JSON snapshot of retrieval hits used
// for that answer so historical citations remain inspectable even after a
// document is reindexed.
type RAGChatTurnRecord struct {
	ID        string          `json:"id"`
	UserID    string          `json:"-"`
	KBID      string          `json:"-"`
	SessionID string          `json:"sessionId"`
	Title     string          `json:"title"`
	Question  string          `json:"question"`
	Answer    string          `json:"answer"`
	Sources   json.RawMessage `json:"-"`
	CreatedAt time.Time       `json:"createdAt"`
}

// RAGChatSessionRecord is derived from rag_chat_turns for the history picker;
// it deliberately has no separate table or mutable session state.
type RAGChatSessionRecord struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	TurnCount int       `json:"turnCount"`
	UpdatedAt time.Time `json:"updatedAt"`
}

const ragKBColumns = `id, user_id, name, description, embed_provider, embed_model,
	embed_dims, chunk_size, chunk_overlap, parse_mode, enrichment_enabled, status, created_at, updated_at`

const ragDocumentColumns = `id, kb_id, file_name, file_type, file_size, object_key,
	status, error_msg, chunk_count, token_count, version, source_sha256, active_version,
	index_format_version, processing_stage, progress_current, progress_total, progress_unit,
	degraded, warning_count, uploaded_at, indexed_at`

const ragIndexTaskColumns = `id, doc_id, doc_version, status, retry_count, max_retry,
	claim_generation, lease_owner, lease_until, heartbeat_at, next_run_at, error_msg,
	created_at, started_at, finished_at`

const ragChatTurnColumns = `id, user_id, kb_id, session_id, title, question,
	answer, sources, created_at`

const ragDocumentVersionColumns = `doc_id, doc_version, status, source_sha256, parse_mode,
	chunk_size, chunk_overlap, parser_version, splitter_version, parse_fingerprint,
	index_fingerprint, vision_model, vision_provider_fingerprint, vision_prompt_version,
	text_model, text_provider_fingerprint, enrichment_prompt_version, enrichment_enabled,
	max_document_ai_requests, max_document_ai_tokens, max_document_ai_cost_microusd,
	embedding_provider, embedding_model, embedding_dimensions, embedding_contract_fingerprint,
	parse_artifact_key, page_count, asset_count, degraded, warning_count, created_at, updated_at`

const ragChunkColumns = `kb_id, doc_id, doc_version, chunk_index, section_title,
	location_json, raw_content, enhancement, search_content, token_count, created_at`

const ragAssetColumns = `id, doc_id, content_sha256, source_kind, source_mime, display_mime,
	source_object_key, display_object_key, thumbnail_object_key, display_status, display_sha256,
	thumbnail_sha256, byte_size, width, height, first_seen_version, last_seen_version, created_at, updated_at`

const ragChunkAssetColumns = `doc_id, doc_version, chunk_index, asset_id, ordinal,
	location_json, caption, ocr_text`

const ragIndexGCTaskColumns = `id, doc_id, retired_version, retired_at, not_before, status,
	claim_generation, lease_owner, lease_until, heartbeat_at, attempt_count, next_run_at, created_at`

const ragDocumentAIUsageColumns = `idempotency_key, logical_request_key, user_id, doc_id,
	task_id, doc_version, claim_generation, lease_owner, operation, provider_fingerprint,
	period_start_utc, reserved_input_tokens, reserved_output_tokens, actual_input_tokens,
	actual_output_tokens, estimated_cost_microusd, state, reservation_expires_at, sent_at,
	usage_estimated, created_at, updated_at`

type ragScanner interface {
	Scan(dest ...any) error
}

type ragExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (d *DBStore) validateRAGVersionSnapshotForDocument(
	ctx context.Context,
	exec ragExecutor,
	doc *RAGDocumentRecord,
	version *RAGDocumentVersionRecord,
) error {
	if doc == nil || version == nil || version.DocID != doc.ID {
		return ErrRAGDocumentVersionMismatch
	}
	if err := validateRunnableRAGVersionSnapshot(version); err != nil {
		return err
	}
	if doc.SourceSHA256 != "" && strings.ToLower(strings.TrimSpace(doc.SourceSHA256)) != version.SourceSHA256 {
		return ErrRAGDocumentSourceConflict
	}
	kb, err := scanRAGKB(exec.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+ragKBColumns+` FROM rag_kbs WHERE id=%s`, d.ph(1)), doc.KBID))
	if err != nil {
		return scanErr(err)
	}
	if version.EmbeddingProvider != kb.EmbedProvider ||
		version.EmbeddingModel != kb.EmbedModel ||
		version.EmbeddingDimensions != kb.EmbedDims {
		return fmt.Errorf("%w: embedding contract differs from knowledge base", ErrRAGDocumentVersionIncomplete)
	}
	return nil
}

func scanRAGKB(scanner ragScanner) (*RAGKBRecord, error) {
	var kb RAGKBRecord
	if err := scanner.Scan(
		&kb.ID, &kb.UserID, &kb.Name, &kb.Description, &kb.EmbedProvider,
		&kb.EmbedModel, &kb.EmbedDims, &kb.ChunkSize, &kb.ChunkOverlap,
		&kb.ParseMode, &kb.EnrichmentEnabled, &kb.Status, &kb.CreatedAt, &kb.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &kb, nil
}

func scanRAGDocument(scanner ragScanner) (*RAGDocumentRecord, error) {
	var doc RAGDocumentRecord
	var indexedAt sql.NullTime
	if err := scanner.Scan(
		&doc.ID, &doc.KBID, &doc.FileName, &doc.FileType, &doc.FileSize,
		&doc.ObjectKey, &doc.Status, &doc.ErrorMsg, &doc.ChunkCount,
		&doc.TokenCount, &doc.Version, &doc.SourceSHA256, &doc.ActiveVersion,
		&doc.IndexFormatVersion, &doc.ProcessingStage, &doc.ProgressCurrent,
		&doc.ProgressTotal, &doc.ProgressUnit, &doc.Degraded, &doc.WarningCount,
		&doc.UploadedAt, &indexedAt,
	); err != nil {
		return nil, err
	}
	if indexedAt.Valid {
		doc.IndexedAt = &indexedAt.Time
	}
	return &doc, nil
}

func scanRAGIndexTask(scanner ragScanner) (*RAGIndexTaskRecord, error) {
	var task RAGIndexTaskRecord
	var leaseUntil, heartbeatAt, nextRunAt, startedAt, finishedAt sql.NullTime
	if err := scanner.Scan(
		&task.ID, &task.DocID, &task.DocVersion, &task.Status, &task.RetryCount,
		&task.MaxRetry, &task.ClaimGeneration, &task.LeaseOwner, &leaseUntil,
		&heartbeatAt, &nextRunAt, &task.ErrorMsg, &task.CreatedAt, &startedAt,
		&finishedAt,
	); err != nil {
		return nil, err
	}
	if leaseUntil.Valid {
		task.LeaseUntil = &leaseUntil.Time
	}
	if heartbeatAt.Valid {
		task.HeartbeatAt = &heartbeatAt.Time
	}
	if nextRunAt.Valid {
		task.NextRunAt = &nextRunAt.Time
	}
	if startedAt.Valid {
		task.StartedAt = &startedAt.Time
	}
	if finishedAt.Valid {
		task.FinishedAt = &finishedAt.Time
	}
	return &task, nil
}

func scanRAGDocumentVersion(scanner ragScanner) (*RAGDocumentVersionRecord, error) {
	var version RAGDocumentVersionRecord
	if err := scanner.Scan(
		&version.DocID, &version.DocVersion, &version.Status, &version.SourceSHA256,
		&version.ParseMode, &version.ChunkSize, &version.ChunkOverlap,
		&version.ParserVersion, &version.SplitterVersion, &version.ParseFingerprint,
		&version.IndexFingerprint, &version.VisionModel, &version.VisionProviderFingerprint,
		&version.VisionPromptVersion, &version.TextModel, &version.TextProviderFingerprint,
		&version.EnrichmentPromptVersion, &version.EnrichmentEnabled,
		&version.MaxDocumentAIRequests, &version.MaxDocumentAITokens,
		&version.MaxDocumentAICostMicroUSD, &version.EmbeddingProvider,
		&version.EmbeddingModel, &version.EmbeddingDimensions,
		&version.EmbeddingContractFingerprint, &version.ParseArtifactKey,
		&version.PageCount, &version.AssetCount, &version.Degraded,
		&version.WarningCount, &version.CreatedAt, &version.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &version, nil
}

func scanRAGChunk(scanner ragScanner) (*RAGChunkRecord, error) {
	var chunk RAGChunkRecord
	if err := scanner.Scan(
		&chunk.KBID, &chunk.DocID, &chunk.DocVersion, &chunk.ChunkIndex,
		&chunk.SectionTitle, &chunk.LocationJSON, &chunk.RawContent,
		&chunk.Enhancement, &chunk.SearchContent, &chunk.TokenCount, &chunk.CreatedAt,
	); err != nil {
		return nil, err
	}
	return &chunk, nil
}

func scanRAGAsset(scanner ragScanner) (*RAGAssetRecord, error) {
	var asset RAGAssetRecord
	if err := scanner.Scan(
		&asset.ID, &asset.DocID, &asset.ContentSHA256, &asset.SourceKind,
		&asset.SourceMIME, &asset.DisplayMIME, &asset.SourceObjectKey,
		&asset.DisplayObjectKey, &asset.ThumbnailObjectKey, &asset.DisplayStatus,
		&asset.DisplaySHA256, &asset.ThumbnailSHA256, &asset.ByteSize, &asset.Width, &asset.Height,
		&asset.FirstSeenVersion, &asset.LastSeenVersion, &asset.CreatedAt, &asset.UpdatedAt,
	); err != nil {
		return nil, err
	}
	return &asset, nil
}

func scanRAGChunkAsset(scanner ragScanner) (*RAGChunkAssetRecord, error) {
	var mapping RAGChunkAssetRecord
	if err := scanner.Scan(
		&mapping.DocID, &mapping.DocVersion, &mapping.ChunkIndex, &mapping.AssetID,
		&mapping.Ordinal, &mapping.LocationJSON, &mapping.Caption, &mapping.OCRText,
	); err != nil {
		return nil, err
	}
	return &mapping, nil
}

func scanRAGIndexGCTask(scanner ragScanner) (*RAGIndexGCTaskRecord, error) {
	var task RAGIndexGCTaskRecord
	var leaseUntil, heartbeatAt, nextRunAt sql.NullTime
	if err := scanner.Scan(
		&task.ID, &task.DocID, &task.RetiredVersion, &task.RetiredAt,
		&task.NotBefore, &task.Status, &task.ClaimGeneration, &task.LeaseOwner,
		&leaseUntil, &heartbeatAt, &task.AttemptCount, &nextRunAt, &task.CreatedAt,
	); err != nil {
		return nil, err
	}
	if leaseUntil.Valid {
		task.LeaseUntil = &leaseUntil.Time
	}
	if heartbeatAt.Valid {
		task.HeartbeatAt = &heartbeatAt.Time
	}
	if nextRunAt.Valid {
		task.NextRunAt = &nextRunAt.Time
	}
	return &task, nil
}

func scanRAGDocumentAIUsage(scanner ragScanner) (*RAGDocumentAIUsageRecord, error) {
	var usage RAGDocumentAIUsageRecord
	var reservationExpiresAt, sentAt sql.NullTime
	var periodStartUTC any
	if err := scanner.Scan(
		&usage.IdempotencyKey, &usage.LogicalRequestKey, &usage.UserID, &usage.DocID,
		&usage.TaskID, &usage.DocVersion, &usage.ClaimGeneration, &usage.LeaseOwner,
		&usage.Operation, &usage.ProviderFingerprint, &periodStartUTC,
		&usage.ReservedInputTokens, &usage.ReservedOutputTokens,
		&usage.ActualInputTokens, &usage.ActualOutputTokens,
		&usage.EstimatedCostMicroUSD, &usage.State, &reservationExpiresAt,
		&sentAt, &usage.UsageEstimated, &usage.CreatedAt, &usage.UpdatedAt,
	); err != nil {
		return nil, err
	}
	period, err := scanRAGDate(periodStartUTC)
	if err != nil {
		return nil, err
	}
	usage.PeriodStartUTC = period
	if reservationExpiresAt.Valid {
		usage.ReservationExpiresAt = &reservationExpiresAt.Time
	}
	if sentAt.Valid {
		usage.SentAt = &sentAt.Time
	}
	return &usage, nil
}

func scanRAGDocumentAITaskBudget(scanner ragScanner) (*RAGDocumentAITaskBudgetRecord, error) {
	var budget RAGDocumentAITaskBudgetRecord
	if err := scanner.Scan(&budget.TaskID, &budget.UserID, &budget.MaxRequests,
		&budget.MaxTokens, &budget.MaxCostMicroUSD, &budget.ChargedRequests,
		&budget.ChargedTokens, &budget.ChargedCostMicroUSD, &budget.UpdatedAt); err != nil {
		return nil, err
	}
	return &budget, nil
}

func scanRAGDocumentAIUserBudget(scanner ragScanner) (*RAGDocumentAIUserBudgetRecord, error) {
	var budget RAGDocumentAIUserBudgetRecord
	var periodStartUTC any
	if err := scanner.Scan(&budget.UserID, &periodStartUTC, &budget.ChargedRequests,
		&budget.ChargedTokens, &budget.ChargedCostMicroUSD, &budget.UpdatedAt); err != nil {
		return nil, err
	}
	period, err := scanRAGDate(periodStartUTC)
	if err != nil {
		return nil, err
	}
	budget.PeriodStartUTC = period
	return &budget, nil
}

func scanRAGDate(value any) (time.Time, error) {
	switch date := value.(type) {
	case time.Time:
		return date.UTC(), nil
	case string:
		parsed, err := time.Parse("2006-01-02", date)
		return parsed.UTC(), err
	case []byte:
		parsed, err := time.Parse("2006-01-02", string(date))
		return parsed.UTC(), err
	default:
		return time.Time{}, fmt.Errorf("store: unsupported SQL DATE value %T", value)
	}
}

func ragPeriodDate(value time.Time) string {
	return value.UTC().Format("2006-01-02")
}

func (d *DBStore) CreateRAGKB(ctx context.Context, kb *RAGKBRecord) error {
	now := time.Now().UTC()
	if kb.CreatedAt.IsZero() {
		kb.CreatedAt = now
	}
	if kb.ParseMode == "" {
		kb.ParseMode = RAGParseModeStandard
	}
	kb.UpdatedAt = now
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_kbs
		(id, user_id, name, description, embed_provider, embed_model, embed_dims,
		 chunk_size, chunk_overlap, parse_mode, enrichment_enabled, status, created_at, updated_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14)),
		kb.ID, kb.UserID, kb.Name, kb.Description, kb.EmbedProvider,
		kb.EmbedModel, kb.EmbedDims, kb.ChunkSize, kb.ChunkOverlap, kb.ParseMode,
		kb.EnrichmentEnabled, kb.Status, kb.CreatedAt, kb.UpdatedAt)
	return err
}

func (d *DBStore) GetRAGKB(ctx context.Context, id string) (*RAGKBRecord, error) {
	kb, err := scanRAGKB(d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+ragKBColumns+` FROM rag_kbs WHERE id = %s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return kb, nil
}

func (d *DBStore) ListRAGKBsByUser(ctx context.Context, userID string) ([]RAGKBRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT `+ragKBColumns+` FROM rag_kbs WHERE user_id = %s ORDER BY created_at, id`,
		d.ph(1)), userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RAGKBRecord
	for rows.Next() {
		kb, err := scanRAGKB(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *kb)
	}
	return out, rows.Err()
}

func (d *DBStore) UpdateRAGKB(ctx context.Context, kb *RAGKBRecord) error {
	kb.UpdatedAt = time.Now().UTC()
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs SET
		name=%s, description=%s, chunk_size=%s, chunk_overlap=%s, parse_mode=%s,
		enrichment_enabled=%s, status=%s, updated_at=%s WHERE id=%s`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9)),
		kb.Name, kb.Description, kb.ChunkSize, kb.ChunkOverlap, kb.ParseMode,
		kb.EnrichmentEnabled, kb.Status, kb.UpdatedAt, kb.ID)
	return ragMutationResult(result, err)
}

func (d *DBStore) DeleteRAGKB(ctx context.Context, id string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM rag_chat_turns WHERE kb_id = %s`, d.ph(1)), id); err != nil {
		return err
	}

	for _, table := range []string{
		"rag_chunk_assets", "rag_chunks", "rag_assets", "rag_index_gc_tasks",
		"rag_document_versions", "rag_index_tasks",
	} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE doc_id IN (SELECT id FROM rag_documents WHERE kb_id = %s)`,
			table, d.ph(1)), id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM rag_documents WHERE kb_id = %s`, d.ph(1)), id); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM rag_kbs WHERE id = %s`, d.ph(1)), id)
	if err != nil {
		return err
	}
	if err := ragMutationResult(result, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) AppendRAGChatTurn(ctx context.Context, turn *RAGChatTurnRecord) error {
	if turn.CreatedAt.IsZero() {
		turn.CreatedAt = time.Now().UTC()
	}
	sources := turn.Sources
	if len(sources) == 0 {
		sources = json.RawMessage("[]")
	}
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_chat_turns
		(id, user_id, kb_id, session_id, title, question, answer, sources, created_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9)),
		turn.ID, turn.UserID, turn.KBID, turn.SessionID, turn.Title,
		turn.Question, turn.Answer, string(sources), turn.CreatedAt)
	return err
}

func scanRAGChatTurn(scanner ragScanner) (*RAGChatTurnRecord, error) {
	var turn RAGChatTurnRecord
	var sources string
	if err := scanner.Scan(
		&turn.ID, &turn.UserID, &turn.KBID, &turn.SessionID, &turn.Title,
		&turn.Question, &turn.Answer, &sources, &turn.CreatedAt,
	); err != nil {
		return nil, err
	}
	turn.Sources = json.RawMessage(sources)
	return &turn, nil
}

func (d *DBStore) ListRAGChatTurns(ctx context.Context, userID, kbID, sessionID string) ([]RAGChatTurnRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT `+ragChatTurnColumns+` FROM rag_chat_turns
		 WHERE user_id = %s AND kb_id = %s AND session_id = %s
		 ORDER BY created_at, id`, d.ph(1), d.ph(2), d.ph(3)),
		userID, kbID, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]RAGChatTurnRecord, 0)
	for rows.Next() {
		turn, err := scanRAGChatTurn(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *turn)
	}
	return out, rows.Err()
}

func (d *DBStore) ListRAGChatSessions(ctx context.Context, userID, kbID string, limit int) ([]RAGChatSessionRecord, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT session_id, MIN(title), COUNT(*), MAX(created_at)
		 FROM rag_chat_turns WHERE user_id = %s AND kb_id = %s
		 GROUP BY session_id
		 ORDER BY MAX(created_at) DESC, session_id DESC LIMIT %s`,
		d.ph(1), d.ph(2), d.ph(3)), userID, kbID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]RAGChatSessionRecord, 0)
	for rows.Next() {
		var session RAGChatSessionRecord
		var updatedAt string
		if err := rows.Scan(&session.ID, &session.Title, &session.TurnCount, &updatedAt); err != nil {
			return nil, err
		}
		session.UpdatedAt = parseTimeString(updatedAt)
		out = append(out, session)
	}
	return out, rows.Err()
}

func (d *DBStore) CreateRAGDocument(ctx context.Context, doc *RAGDocumentRecord) error {
	return d.createRAGDocument(ctx, d.db, doc)
}

func (d *DBStore) createRAGDocument(ctx context.Context, exec ragExecutor, doc *RAGDocumentRecord) error {
	if doc.UploadedAt.IsZero() {
		doc.UploadedAt = time.Now().UTC()
	}
	if doc.IndexFormatVersion == 0 {
		doc.IndexFormatVersion = 1
	}
	if doc.ProcessingStage == "" {
		doc.ProcessingStage = "queued"
	}
	_, err := exec.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_documents
		(id, kb_id, file_name, file_type, file_size, object_key, status, error_msg,
		 chunk_count, token_count, version, source_sha256, active_version,
		 index_format_version, processing_stage, progress_current, progress_total,
		 progress_unit, degraded, warning_count, uploaded_at, indexed_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s,
		 %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7),
		d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13),
		d.ph(14), d.ph(15), d.ph(16), d.ph(17), d.ph(18), d.ph(19),
		d.ph(20), d.ph(21), d.ph(22)),
		doc.ID, doc.KBID, doc.FileName, doc.FileType, doc.FileSize, doc.ObjectKey,
		doc.Status, doc.ErrorMsg, doc.ChunkCount, doc.TokenCount, doc.Version,
		doc.SourceSHA256, doc.ActiveVersion, doc.IndexFormatVersion,
		doc.ProcessingStage, doc.ProgressCurrent, doc.ProgressTotal, doc.ProgressUnit,
		doc.Degraded, doc.WarningCount, doc.UploadedAt, doc.IndexedAt)
	return err
}

func ragAdvancedVersion(version *RAGDocumentVersionRecord) bool {
	return version != nil && (version.ParseMode == RAGParseModeAuto || version.EnrichmentEnabled)
}

func (d *DBStore) lockRAGAdvancedEnqueueUserTx(
	ctx context.Context,
	tx *sql.Tx,
	policy *RAGAdvancedEnqueuePolicy,
) error {
	if policy == nil || policy.UserID == "" || policy.UserID != strings.TrimSpace(policy.UserID) ||
		policy.MaxPendingTasks <= 0 || policy.MinReindexInterval < 0 {
		return errors.New("store: invalid RAG advanced enqueue policy")
	}
	_, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_kbs
		SET updated_at=updated_at WHERE user_id=%s`, d.ph(1)), policy.UserID)
	return err
}

func (d *DBStore) enforceRAGAdvancedEnqueuePolicyTx(
	ctx context.Context,
	tx *sql.Tx,
	kbID, docID string,
	version *RAGDocumentVersionRecord,
	policy *RAGAdvancedEnqueuePolicy,
	reindex bool,
) error {
	if policy == nil || !ragAdvancedVersion(version) {
		return nil
	}
	// Every compliant enqueue for this user writes the same set of KB rows
	// before reading the outstanding count. This is a database-backed per-user
	// mutex on SQLite and row locks on PostgreSQL/MySQL, so two processes cannot
	// both admit the last slot after a racy count.
	if err := d.lockRAGAdvancedEnqueueUserTx(ctx, tx, policy); err != nil {
		return err
	}
	var ownerID string
	if err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT user_id FROM rag_kbs WHERE id=%s`, d.ph(1)), kbID).Scan(&ownerID); err != nil {
		return scanErr(err)
	}
	if ownerID != policy.UserID {
		return ErrRAGDocumentVersionMismatch
	}

	var pending int
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COUNT(*) FROM rag_index_tasks t
		JOIN rag_document_versions v ON v.doc_id=t.doc_id AND v.doc_version=t.doc_version
		JOIN rag_documents d ON d.id=t.doc_id
		JOIN rag_kbs k ON k.id=d.kb_id
		WHERE k.user_id=%s AND t.status IN ('PENDING','RUNNING')
		AND (v.parse_mode='auto' OR v.enrichment_enabled=TRUE)`, d.ph(1)), policy.UserID).Scan(&pending)
	if err != nil {
		return err
	}
	if pending >= policy.MaxPendingTasks {
		return ErrRAGAdvancedPendingLimit
	}

	if !reindex || policy.MinReindexInterval == 0 {
		return nil
	}
	var latest time.Time
	err = tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT t.created_at FROM rag_index_tasks t
		JOIN rag_document_versions v ON v.doc_id=t.doc_id AND v.doc_version=t.doc_version
		WHERE t.doc_id=%s AND (v.parse_mode='auto' OR v.enrichment_enabled=TRUE)
		ORDER BY t.created_at DESC LIMIT 1`, d.ph(1)), docID).Scan(&latest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	dbNow, err := d.ragDBNow(ctx, tx)
	if err != nil {
		return err
	}
	if latest.Add(policy.MinReindexInterval).After(dbNow) {
		return ErrRAGAdvancedReindexRateLimit
	}
	return nil
}

// CreateRAGDocumentWithVersionAndIndexTask atomically creates the document,
// its immutable version snapshot, and the durable index task. This low-level
// compatibility helper does not apply the optional advanced enqueue policy.
func (d *DBStore) CreateRAGDocumentWithVersionAndIndexTask(
	ctx context.Context,
	doc *RAGDocumentRecord,
	version *RAGDocumentVersionRecord,
	maxRetry int,
) (int64, error) {
	return d.createRAGDocumentWithVersionAndIndexTask(ctx, doc, version, maxRetry, nil)
}

func (d *DBStore) CreateRAGDocumentWithVersionAndIndexTaskPolicy(
	ctx context.Context,
	doc *RAGDocumentRecord,
	version *RAGDocumentVersionRecord,
	maxRetry int,
	policy RAGAdvancedEnqueuePolicy,
) (int64, error) {
	return d.createRAGDocumentWithVersionAndIndexTask(ctx, doc, version, maxRetry, &policy)
}

func (d *DBStore) createRAGDocumentWithVersionAndIndexTask(
	ctx context.Context,
	doc *RAGDocumentRecord,
	version *RAGDocumentVersionRecord,
	maxRetry int,
	policy *RAGAdvancedEnqueuePolicy,
) (int64, error) {
	if doc == nil || version == nil || version.DocID != doc.ID || version.DocVersion != doc.Version {
		return 0, ErrRAGDocumentVersionMismatch
	}
	prepareNewRAGDocumentVersion(version)
	normalizedSource, fillSource, err := ragDocumentSourceHash(doc.SourceSHA256, version.SourceSHA256)
	if err != nil {
		return 0, err
	}
	if fillSource {
		doc.SourceSHA256 = normalizedSource
	}
	if err := d.validateRAGVersionSnapshotForDocument(ctx, d.db, doc, version); err != nil {
		return 0, err
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := d.enforceRAGAdvancedEnqueuePolicyTx(ctx, tx, doc.KBID, doc.ID, version, policy, false); err != nil {
		return 0, err
	}
	if err := d.createRAGDocument(ctx, tx, doc); err != nil {
		return 0, err
	}
	if err := d.createRAGDocumentVersion(ctx, tx, version); err != nil {
		return 0, err
	}
	taskID, err := d.createRAGIndexTask(ctx, tx, doc.ID, maxRetry)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return taskID, nil
}

func (d *DBStore) CreateRAGDocumentVersion(ctx context.Context, version *RAGDocumentVersionRecord) error {
	if version == nil || version.DocID == "" {
		return ErrRAGDocumentVersionMismatch
	}
	prepareNewRAGDocumentVersion(version)
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	doc, err := d.ragDocumentInTx(ctx, tx, version.DocID)
	if err != nil {
		return scanErr(err)
	}
	if err := d.reconcileRAGDocumentSourceHash(ctx, tx, doc, version.SourceSHA256); err != nil {
		return err
	}
	if err := d.validateRAGVersionSnapshotForDocument(ctx, tx, doc, version); err != nil {
		return err
	}
	if err := d.createRAGDocumentVersion(ctx, tx, version); err != nil {
		return err
	}
	return tx.Commit()
}

func prepareNewRAGDocumentVersion(version *RAGDocumentVersionRecord) {
	version.Status = RAGDocumentVersionPending
	version.ParseArtifactKey = ""
	version.PageCount = 0
	version.AssetCount = 0
	version.Degraded = false
	version.WarningCount = 0
}

func (d *DBStore) createRAGDocumentVersion(
	ctx context.Context,
	exec ragExecutor,
	version *RAGDocumentVersionRecord,
) error {
	now := time.Now().UTC()
	if version.CreatedAt.IsZero() {
		version.CreatedAt = now
	}
	version.UpdatedAt = now
	if version.Status == "" {
		version.Status = RAGDocumentVersionPending
	}
	_, err := exec.ExecContext(ctx, fmt.Sprintf(`INSERT INTO rag_document_versions (
		doc_id, doc_version, status, source_sha256, parse_mode, chunk_size,
		chunk_overlap, parser_version, splitter_version, parse_fingerprint,
		index_fingerprint, vision_model, vision_provider_fingerprint,
		vision_prompt_version, text_model, text_provider_fingerprint,
		enrichment_prompt_version, enrichment_enabled, max_document_ai_requests,
		max_document_ai_tokens, max_document_ai_cost_microusd, embedding_provider,
		embedding_model, embedding_dimensions, embedding_contract_fingerprint,
		parse_artifact_key, page_count, asset_count, degraded, warning_count,
		created_at, updated_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s,
		%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s,
		%s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7),
		d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12), d.ph(13), d.ph(14),
		d.ph(15), d.ph(16), d.ph(17), d.ph(18), d.ph(19), d.ph(20), d.ph(21),
		d.ph(22), d.ph(23), d.ph(24), d.ph(25), d.ph(26), d.ph(27), d.ph(28),
		d.ph(29), d.ph(30), d.ph(31), d.ph(32)),
		version.DocID, version.DocVersion, version.Status, version.SourceSHA256,
		version.ParseMode, version.ChunkSize, version.ChunkOverlap,
		version.ParserVersion, version.SplitterVersion, version.ParseFingerprint,
		version.IndexFingerprint, version.VisionModel,
		version.VisionProviderFingerprint, version.VisionPromptVersion,
		version.TextModel, version.TextProviderFingerprint,
		version.EnrichmentPromptVersion, version.EnrichmentEnabled,
		version.MaxDocumentAIRequests, version.MaxDocumentAITokens,
		version.MaxDocumentAICostMicroUSD, version.EmbeddingProvider,
		version.EmbeddingModel, version.EmbeddingDimensions,
		version.EmbeddingContractFingerprint, version.ParseArtifactKey,
		version.PageCount, version.AssetCount, version.Degraded,
		version.WarningCount, version.CreatedAt, version.UpdatedAt)
	return err
}

func (d *DBStore) GetRAGDocumentVersion(ctx context.Context, docID string, docVersion int64) (*RAGDocumentVersionRecord, error) {
	version, err := scanRAGDocumentVersion(d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragDocumentVersionColumns+` FROM rag_document_versions
		 WHERE doc_id = %s AND doc_version = %s`, d.ph(1), d.ph(2)), docID, docVersion))
	if err != nil {
		return nil, scanErr(err)
	}
	return version, nil
}

func (d *DBStore) ListRAGDocumentVersions(ctx context.Context, docID string) ([]RAGDocumentVersionRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT `+ragDocumentVersionColumns+` FROM rag_document_versions
		 WHERE doc_id = %s ORDER BY doc_version`, d.ph(1)), docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	versions := make([]RAGDocumentVersionRecord, 0)
	for rows.Next() {
		version, err := scanRAGDocumentVersion(rows)
		if err != nil {
			return nil, err
		}
		versions = append(versions, *version)
	}
	return versions, rows.Err()
}

func (d *DBStore) GetRAGDocument(ctx context.Context, id string) (*RAGDocumentRecord, error) {
	doc, err := scanRAGDocument(d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+ragDocumentColumns+` FROM rag_documents WHERE id = %s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return doc, nil
}

func (d *DBStore) ListRAGDocumentsByKB(ctx context.Context, kbID string) ([]RAGDocumentRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT `+ragDocumentColumns+` FROM rag_documents WHERE kb_id = %s ORDER BY uploaded_at, id`,
		d.ph(1)), kbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RAGDocumentRecord
	for rows.Next() {
		doc, err := scanRAGDocument(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *doc)
	}
	return out, rows.Err()
}

func (d *DBStore) DeleteRAGDocument(ctx context.Context, id string) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Usage and aggregate budget ledgers are intentionally retained for audit
	// and period accounting even after the source document is deleted.
	for _, table := range []string{
		"rag_chunk_assets", "rag_chunks", "rag_assets", "rag_index_gc_tasks",
		"rag_document_versions", "rag_index_tasks",
	} {
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE doc_id = %s`, table, d.ph(1)), id); err != nil {
			return err
		}
	}
	result, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM rag_documents WHERE id = %s`, d.ph(1)), id)
	if err != nil {
		return err
	}
	if err := ragMutationResult(result, nil); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DBStore) createRAGIndexTask(ctx context.Context, exec ragExecutor, docID string, maxRetry int) (int64, error) {
	var docVersion int64
	if err := exec.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT version FROM rag_documents WHERE id=%s`, d.ph(1)), docID).Scan(&docVersion); err != nil {
		return 0, scanErr(err)
	}
	return d.createRAGIndexTaskForVersion(ctx, exec, docID, docVersion, maxRetry)
}

func (d *DBStore) createRAGIndexTaskForVersion(
	ctx context.Context,
	exec ragExecutor,
	docID string,
	docVersion int64,
	maxRetry int,
) (int64, error) {
	if maxRetry <= 0 {
		maxRetry = 3
	}
	if d.dialect == "postgres" {
		var id int64
		err := exec.QueryRowContext(ctx, fmt.Sprintf(`INSERT INTO rag_index_tasks
			(doc_id, doc_version, status, retry_count, max_retry, claim_generation,
			 lease_owner, error_msg, created_at)
			VALUES (%s, %s, 'PENDING', 0, %s, 0, '', '', CURRENT_TIMESTAMP) RETURNING id`,
			d.ph(1), d.ph(2), d.ph(3)), docID, docVersion, maxRetry).Scan(&id)
		return id, err
	}

	result, err := exec.ExecContext(ctx, `INSERT INTO rag_index_tasks
		(doc_id, doc_version, status, retry_count, max_retry, claim_generation,
		 lease_owner, error_msg, created_at)
		VALUES (?, ?, 'PENDING', 0, ?, 0, '', '', CURRENT_TIMESTAMP)`,
		docID, docVersion, maxRetry)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (d *DBStore) GetRAGIndexTask(ctx context.Context, id int64) (*RAGIndexTaskRecord, error) {
	task, err := scanRAGIndexTask(d.db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT `+ragIndexTaskColumns+` FROM rag_index_tasks WHERE id = %s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return task, nil
}

func ragMutationResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DBStore) ListRunnableRAGIndexTasks(ctx context.Context) ([]RAGIndexTaskRecord, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT `+ragIndexTaskColumns+`
		FROM rag_index_tasks WHERE status IN ('PENDING', 'RUNNING') ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []RAGIndexTaskRecord
	for rows.Next() {
		task, err := scanRAGIndexTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *task)
	}
	return out, rows.Err()
}

func (d *DBStore) PutRAGChunks(ctx context.Context, chunks []RAGChunkRecord) error {
	if len(chunks) == 0 {
		return nil
	}
	if len(chunks) > maxRAGBatchRecords {
		return ErrRAGBatchTooLarge
	}
	query := fmt.Sprintf(`INSERT INTO rag_chunks (
		kb_id, doc_id, doc_version, chunk_index, section_title, location_json,
		raw_content, enhancement, search_content, token_count, created_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11))
	if d.dialect == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE
			kb_id=VALUES(kb_id), section_title=VALUES(section_title),
			location_json=VALUES(location_json), raw_content=VALUES(raw_content),
			enhancement=VALUES(enhancement), search_content=VALUES(search_content),
			token_count=VALUES(token_count), created_at=VALUES(created_at)`
	} else {
		query += ` ON CONFLICT (doc_id, doc_version, chunk_index) DO UPDATE SET
			kb_id=excluded.kb_id, section_title=excluded.section_title,
			location_json=excluded.location_json, raw_content=excluded.raw_content,
			enhancement=excluded.enhancement, search_content=excluded.search_content,
			token_count=excluded.token_count, created_at=excluded.created_at`
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := time.Now().UTC()
	for i := range chunks {
		chunk := &chunks[i]
		if chunk.CreatedAt.IsZero() {
			chunk.CreatedAt = now
		}
		if _, err := stmt.ExecContext(ctx, chunk.KBID, chunk.DocID,
			chunk.DocVersion, chunk.ChunkIndex, chunk.SectionTitle,
			chunk.LocationJSON, chunk.RawContent, chunk.Enhancement,
			chunk.SearchContent, chunk.TokenCount, chunk.CreatedAt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DBStore) ListRAGChunksByRefs(ctx context.Context, refs []RAGChunkRef) ([]RAGChunkRecord, error) {
	if len(refs) == 0 {
		return []RAGChunkRecord{}, nil
	}
	predicate, args, err := d.ragChunkRefPredicate(refs)
	if err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+ragChunkColumns+` FROM rag_chunks WHERE `+
		predicate+` ORDER BY doc_id, doc_version, chunk_index`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGChunkRecord, 0, len(refs))
	for rows.Next() {
		chunk, err := scanRAGChunk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *chunk)
	}
	return out, rows.Err()
}

func (d *DBStore) ListRAGChunksByDocumentVersion(ctx context.Context, docID string, docVersion int64) ([]RAGChunkRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT `+ragChunkColumns+` FROM rag_chunks WHERE doc_id=%s AND doc_version=%s
		 ORDER BY chunk_index`, d.ph(1), d.ph(2)), docID, docVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGChunkRecord, 0)
	for rows.Next() {
		chunk, err := scanRAGChunk(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *chunk)
	}
	return out, rows.Err()
}

func (d *DBStore) DeleteRAGChunksByDocumentVersion(ctx context.Context, docID string, docVersion int64) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM rag_chunks WHERE doc_id=%s AND doc_version=%s`,
		d.ph(1), d.ph(2)), docID, docVersion)
	return err
}

func (d *DBStore) PutRAGChunkAssets(ctx context.Context, mappings []RAGChunkAssetRecord) error {
	if len(mappings) == 0 {
		return nil
	}
	if len(mappings) > maxRAGBatchRecords {
		return ErrRAGBatchTooLarge
	}
	query := fmt.Sprintf(`INSERT INTO rag_chunk_assets (
		doc_id, doc_version, chunk_index, asset_id, ordinal, location_json, caption, ocr_text)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s)`, d.ph(1), d.ph(2), d.ph(3),
		d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8))
	if d.dialect == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE location_json=VALUES(location_json),
			caption=VALUES(caption), ocr_text=VALUES(ocr_text)`
	} else {
		query += ` ON CONFLICT (doc_id, doc_version, chunk_index, asset_id, ordinal)
			DO UPDATE SET location_json=excluded.location_json,
			caption=excluded.caption, ocr_text=excluded.ocr_text`
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for i := range mappings {
		mapping := &mappings[i]
		if _, err := stmt.ExecContext(ctx, mapping.DocID, mapping.DocVersion,
			mapping.ChunkIndex, mapping.AssetID, mapping.Ordinal,
			mapping.LocationJSON, mapping.Caption, mapping.OCRText); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DBStore) ListRAGChunkAssetsByRefs(ctx context.Context, refs []RAGChunkRef) ([]RAGChunkAssetRecord, error) {
	if len(refs) == 0 {
		return []RAGChunkAssetRecord{}, nil
	}
	predicate, args, err := d.ragChunkRefPredicate(refs)
	if err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+ragChunkAssetColumns+
		` FROM rag_chunk_assets WHERE `+predicate+
		` ORDER BY doc_id, doc_version, chunk_index, ordinal, asset_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGChunkAssetRecord, 0)
	for rows.Next() {
		mapping, err := scanRAGChunkAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *mapping)
	}
	return out, rows.Err()
}

func (d *DBStore) DeleteRAGChunkAssetsByDocumentVersion(ctx context.Context, docID string, docVersion int64) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM rag_chunk_assets WHERE doc_id=%s AND doc_version=%s`,
		d.ph(1), d.ph(2)), docID, docVersion)
	return err
}

func (d *DBStore) ragChunkRefPredicate(refs []RAGChunkRef) (string, []any, error) {
	if len(refs) > maxRAGBatchRecords {
		return "", nil, ErrRAGBatchTooLarge
	}
	var predicate strings.Builder
	args := make([]any, 0, len(refs)*3)
	for i, ref := range refs {
		if i > 0 {
			predicate.WriteString(" OR ")
		}
		base := len(args) + 1
		fmt.Fprintf(&predicate, "(doc_id=%s AND doc_version=%s AND chunk_index=%s)",
			d.ph(base), d.ph(base+1), d.ph(base+2))
		args = append(args, ref.DocID, ref.DocVersion, ref.ChunkIndex)
	}
	return predicate.String(), args, nil
}

func (d *DBStore) UpsertRAGAsset(ctx context.Context, asset *RAGAssetRecord) error {
	now := time.Now().UTC()
	if asset.CreatedAt.IsZero() {
		asset.CreatedAt = now
	}
	if asset.UpdatedAt.IsZero() {
		asset.UpdatedAt = now
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO rag_assets (
		id, doc_id, content_sha256, source_kind, source_mime, display_mime,
		source_object_key, display_object_key, thumbnail_object_key, display_status,
		display_sha256, thumbnail_sha256, byte_size, width, height, first_seen_version,
		last_seen_version, created_at, updated_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s,
		%s, %s, %s, %s, %s, %s)`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5),
		d.ph(6), d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12),
		d.ph(13), d.ph(14), d.ph(15), d.ph(16), d.ph(17), d.ph(18), d.ph(19))
	if d.dialect == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE id=id`
	} else {
		query += ` ON CONFLICT DO NOTHING`
	}
	if _, err := tx.ExecContext(ctx, query, asset.ID, asset.DocID,
		asset.ContentSHA256, asset.SourceKind, asset.SourceMIME, asset.DisplayMIME,
		asset.SourceObjectKey, asset.DisplayObjectKey, asset.ThumbnailObjectKey,
		asset.DisplayStatus, asset.DisplaySHA256, asset.ThumbnailSHA256, asset.ByteSize, asset.Width,
		asset.Height, asset.FirstSeenVersion, asset.LastSeenVersion,
		asset.CreatedAt, asset.UpdatedAt); err != nil {
		return err
	}
	existing, err := scanRAGAsset(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragAssetColumns+` FROM rag_assets WHERE doc_id=%s AND content_sha256=%s`,
		d.ph(1), d.ph(2)), asset.DocID, asset.ContentSHA256))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrRAGAssetConflict
	}
	if err != nil {
		return err
	}
	// Phase C added the exact thumbnail-byte hash needed by conditional asset
	// responses. Phase B rows have an empty value after the additive migration;
	// allow exactly one canonical, compare-and-set backfill. Concurrent rebuilds
	// must agree or the immutable comparison below rejects the loser.
	if existing.ThumbnailSHA256 == "" && asset.ThumbnailSHA256 != "" {
		if !ragCanonicalSHA256(asset.ThumbnailSHA256) {
			return ErrRAGAssetConflict
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_assets
			SET thumbnail_sha256=%s WHERE id=%s AND thumbnail_sha256=''`, d.ph(1), d.ph(2)),
			asset.ThumbnailSHA256, existing.ID); err != nil {
			return err
		}
		existing, err = scanRAGAsset(tx.QueryRowContext(ctx, fmt.Sprintf(
			`SELECT `+ragAssetColumns+` FROM rag_assets WHERE id=%s`, d.ph(1)), existing.ID))
		if err != nil {
			return err
		}
	}
	if !sameImmutableRAGAsset(existing, asset) {
		return ErrRAGAssetConflict
	}
	updatedAt := time.Now().UTC()
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE rag_assets SET
		first_seen_version=CASE WHEN first_seen_version < %s THEN first_seen_version ELSE %s END,
		last_seen_version=CASE WHEN last_seen_version > %s THEN last_seen_version ELSE %s END,
		updated_at=%s WHERE id=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6)),
		asset.FirstSeenVersion, asset.FirstSeenVersion, asset.LastSeenVersion,
		asset.LastSeenVersion, updatedAt, existing.ID)
	if err := ragMutationResult(result, err); err != nil {
		return err
	}
	asset.FirstSeenVersion = minInt64(existing.FirstSeenVersion, asset.FirstSeenVersion)
	asset.LastSeenVersion = maxInt64(existing.LastSeenVersion, asset.LastSeenVersion)
	asset.CreatedAt = existing.CreatedAt
	asset.UpdatedAt = updatedAt
	return tx.Commit()
}

func sameImmutableRAGAsset(a, b *RAGAssetRecord) bool {
	return a.ID == b.ID && a.DocID == b.DocID &&
		a.ContentSHA256 == b.ContentSHA256 && a.SourceKind == b.SourceKind &&
		a.SourceMIME == b.SourceMIME && a.DisplayMIME == b.DisplayMIME &&
		a.SourceObjectKey == b.SourceObjectKey && a.DisplayObjectKey == b.DisplayObjectKey &&
		a.ThumbnailObjectKey == b.ThumbnailObjectKey && a.DisplayStatus == b.DisplayStatus &&
		a.DisplaySHA256 == b.DisplaySHA256 && a.ThumbnailSHA256 == b.ThumbnailSHA256 && a.ByteSize == b.ByteSize &&
		a.Width == b.Width && a.Height == b.Height
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (d *DBStore) GetRAGAsset(ctx context.Context, id string) (*RAGAssetRecord, error) {
	asset, err := scanRAGAsset(d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragAssetColumns+` FROM rag_assets WHERE id=%s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return asset, nil
}

func (d *DBStore) ListRAGAssetsByIDs(ctx context.Context, ids []string) ([]RAGAssetRecord, error) {
	if len(ids) == 0 {
		return []RAGAssetRecord{}, nil
	}
	if len(ids) > maxRAGBatchRecords {
		return nil, ErrRAGBatchTooLarge
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = d.ph(i + 1)
		args[i] = id
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+ragAssetColumns+
		` FROM rag_assets WHERE id IN (`+strings.Join(placeholders, ",")+`) ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGAssetRecord, 0, len(ids))
	for rows.Next() {
		asset, err := scanRAGAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *asset)
	}
	return out, rows.Err()
}

func (d *DBStore) ListRAGAssetsByChunkRefs(ctx context.Context, refs []RAGChunkRef) ([]RAGAssetRecord, error) {
	mappings, err := d.ListRAGChunkAssetsByRefs(ctx, refs)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(mappings))
	ids := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		if _, exists := seen[mapping.AssetID]; exists {
			continue
		}
		seen[mapping.AssetID] = struct{}{}
		ids = append(ids, mapping.AssetID)
	}
	assets := make([]RAGAssetRecord, 0, len(ids))
	for start := 0; start < len(ids); start += maxRAGBatchRecords {
		end := min(start+maxRAGBatchRecords, len(ids))
		batch, err := d.ListRAGAssetsByIDs(ctx, ids[start:end])
		if err != nil {
			return nil, err
		}
		assets = append(assets, batch...)
	}
	return assets, nil
}

func (d *DBStore) ListRAGAssetsByDocument(ctx context.Context, docID string) ([]RAGAssetRecord, error) {
	rows, err := d.db.QueryContext(ctx, fmt.Sprintf(
		`SELECT `+ragAssetColumns+` FROM rag_assets WHERE doc_id=%s ORDER BY id`, d.ph(1)), docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGAssetRecord, 0)
	for rows.Next() {
		asset, err := scanRAGAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *asset)
	}
	return out, rows.Err()
}

func (d *DBStore) DeleteRAGAssetsByDocument(ctx context.Context, docID string) error {
	_, err := d.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM rag_assets WHERE doc_id=%s`, d.ph(1)), docID)
	return err
}

func (d *DBStore) CreateRAGIndexGCTask(ctx context.Context, task *RAGIndexGCTaskRecord) (int64, error) {
	now := time.Now().UTC()
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	if task.RetiredAt.IsZero() {
		task.RetiredAt = now
	}
	if task.NotBefore.IsZero() {
		task.NotBefore = now
	}
	if task.Status == "" {
		task.Status = "PENDING"
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	query := fmt.Sprintf(`INSERT INTO rag_index_gc_tasks (
		doc_id, retired_version, retired_at, not_before, status, claim_generation,
		lease_owner, lease_until, heartbeat_at, attempt_count, next_run_at, created_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)`,
		d.ph(1), d.ph(2), d.ph(3), d.ph(4), d.ph(5), d.ph(6),
		d.ph(7), d.ph(8), d.ph(9), d.ph(10), d.ph(11), d.ph(12))
	if d.dialect == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE id=id`
	} else {
		query += ` ON CONFLICT (doc_id, retired_version) DO NOTHING`
	}
	if _, err := tx.ExecContext(ctx, query, task.DocID, task.RetiredVersion,
		task.RetiredAt, task.NotBefore, task.Status, task.ClaimGeneration,
		task.LeaseOwner, task.LeaseUntil, task.HeartbeatAt, task.AttemptCount,
		task.NextRunAt, task.CreatedAt); err != nil {
		return 0, err
	}
	stored, err := scanRAGIndexGCTask(tx.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragIndexGCTaskColumns+` FROM rag_index_gc_tasks
		 WHERE doc_id=%s AND retired_version=%s`, d.ph(1), d.ph(2)),
		task.DocID, task.RetiredVersion))
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	task.ID = stored.ID
	return stored.ID, nil
}

func (d *DBStore) GetRAGIndexGCTask(ctx context.Context, id int64) (*RAGIndexGCTaskRecord, error) {
	task, err := scanRAGIndexGCTask(d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragIndexGCTaskColumns+` FROM rag_index_gc_tasks WHERE id=%s`, d.ph(1)), id))
	if err != nil {
		return nil, scanErr(err)
	}
	return task, nil
}

func (d *DBStore) ListRAGIndexGCTasks(ctx context.Context, status string, limit int) ([]RAGIndexGCTaskRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > maxRAGBatchRecords {
		limit = maxRAGBatchRecords
	}
	query := `SELECT ` + ragIndexGCTaskColumns + ` FROM rag_index_gc_tasks`
	args := make([]any, 0, 2)
	if status != "" {
		query += ` WHERE status=` + d.ph(1)
		args = append(args, status)
	}
	query += ` ORDER BY not_before, created_at, id LIMIT ` + d.ph(len(args)+1)
	args = append(args, limit)
	rows, err := d.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]RAGIndexGCTaskRecord, 0)
	for rows.Next() {
		task, err := scanRAGIndexGCTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *task)
	}
	return out, rows.Err()
}

// UpdateRAGIndexGCTaskState is a status CAS for scheduler/system transitions.
// Lease ownership checks are layered on by the GC claim implementation.
func (d *DBStore) UpdateRAGIndexGCTaskState(
	ctx context.Context,
	id int64,
	expectedStatus, status string,
	nextRunAt *time.Time,
) (bool, error) {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`UPDATE rag_index_gc_tasks SET status=%s, next_run_at=%s
		 WHERE id=%s AND status=%s`, d.ph(1), d.ph(2), d.ph(3), d.ph(4)),
		status, nextRunAt, id, expectedStatus)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

func (d *DBStore) DeleteRAGIndexGCTask(ctx context.Context, id int64) error {
	result, err := d.db.ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM rag_index_gc_tasks WHERE id=%s`, d.ph(1)), id)
	return ragMutationResult(result, err)
}

func (d *DBStore) CreateRAGDocumentAITaskBudget(ctx context.Context, budget *RAGDocumentAITaskBudgetRecord) error {
	if budget == nil || budget.TaskID <= 0 || strings.TrimSpace(budget.UserID) == "" ||
		budget.MaxRequests < 0 || budget.MaxTokens < 0 || budget.MaxCostMicroUSD < 0 ||
		budget.ChargedRequests < 0 || budget.ChargedTokens < 0 || budget.ChargedCostMicroUSD < 0 {
		return errors.New("store: invalid RAG DocumentAI task budget")
	}
	if budget.UpdatedAt.IsZero() {
		budget.UpdatedAt = time.Now().UTC()
	}
	query := fmt.Sprintf(`INSERT INTO rag_document_ai_task_budgets (
		task_id, user_id, max_requests, max_tokens, max_cost_microusd,
		charged_requests, charged_tokens, charged_cost_microusd, updated_at)
		VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)`, d.ph(1), d.ph(2),
		d.ph(3), d.ph(4), d.ph(5), d.ph(6), d.ph(7), d.ph(8), d.ph(9))
	if d.dialect == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE task_id=task_id`
	} else {
		query += ` ON CONFLICT (task_id) DO NOTHING`
	}
	_, err := d.db.ExecContext(ctx, query, budget.TaskID, budget.UserID,
		budget.MaxRequests, budget.MaxTokens, budget.MaxCostMicroUSD,
		budget.ChargedRequests, budget.ChargedTokens,
		budget.ChargedCostMicroUSD, budget.UpdatedAt)
	if err != nil {
		return err
	}
	// INSERT .. DO NOTHING is the crash/reclaim idempotency path, not
	// permission to silently reuse a task ID with another immutable snapshot.
	// Verify the existing row after the insert race has settled.
	existing, err := d.GetRAGDocumentAITaskBudget(ctx, budget.TaskID)
	if err != nil {
		return err
	}
	if existing.UserID != budget.UserID || existing.MaxRequests != budget.MaxRequests ||
		existing.MaxTokens != budget.MaxTokens || existing.MaxCostMicroUSD != budget.MaxCostMicroUSD {
		return ErrRAGDocumentAIUsageConflict
	}
	return nil
}

func (d *DBStore) GetRAGDocumentAITaskBudget(ctx context.Context, taskID int64) (*RAGDocumentAITaskBudgetRecord, error) {
	budget, err := scanRAGDocumentAITaskBudget(d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT task_id, user_id, max_requests, max_tokens, max_cost_microusd,
		 charged_requests, charged_tokens, charged_cost_microusd, updated_at
		 FROM rag_document_ai_task_budgets WHERE task_id=%s`, d.ph(1)), taskID))
	if err != nil {
		return nil, scanErr(err)
	}
	return budget, nil
}

func (d *DBStore) CreateRAGDocumentAIUserBudget(ctx context.Context, budget *RAGDocumentAIUserBudgetRecord) error {
	if budget.UpdatedAt.IsZero() {
		budget.UpdatedAt = time.Now().UTC()
	}
	query := fmt.Sprintf(`INSERT INTO rag_document_ai_user_budgets (
		user_id, period_start_utc, charged_requests, charged_tokens,
		charged_cost_microusd, updated_at)
		VALUES (%s, %s, %s, %s, %s, %s)`, d.ph(1), d.ph(2), d.ph(3),
		d.ph(4), d.ph(5), d.ph(6))
	if d.dialect == "mysql" {
		query += ` ON DUPLICATE KEY UPDATE user_id=user_id`
	} else {
		query += ` ON CONFLICT (user_id, period_start_utc) DO NOTHING`
	}
	_, err := d.db.ExecContext(ctx, query, budget.UserID,
		ragPeriodDate(budget.PeriodStartUTC), budget.ChargedRequests,
		budget.ChargedTokens, budget.ChargedCostMicroUSD, budget.UpdatedAt)
	return err
}

func (d *DBStore) GetRAGDocumentAIUserBudget(
	ctx context.Context,
	userID string,
	periodStartUTC time.Time,
) (*RAGDocumentAIUserBudgetRecord, error) {
	budget, err := scanRAGDocumentAIUserBudget(d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT user_id, period_start_utc, charged_requests, charged_tokens,
		 charged_cost_microusd, updated_at FROM rag_document_ai_user_budgets
		 WHERE user_id=%s AND period_start_utc=%s`, d.ph(1), d.ph(2)),
		userID, ragPeriodDate(periodStartUTC)))
	if err != nil {
		return nil, scanErr(err)
	}
	return budget, nil
}

func (d *DBStore) GetRAGDocumentAIUsage(ctx context.Context, idempotencyKey string) (*RAGDocumentAIUsageRecord, error) {
	usage, err := scanRAGDocumentAIUsage(d.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT `+ragDocumentAIUsageColumns+` FROM rag_document_ai_usage
		 WHERE idempotency_key=%s`, d.ph(1)), idempotencyKey))
	if err != nil {
		return nil, scanErr(err)
	}
	return usage, nil
}
