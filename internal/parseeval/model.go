// Package parseeval contains the document-parser evaluation domain. It is
// intentionally independent from internal/rag/eval: parser evaluation stops
// before chunking and has different evidence, state, and reproducibility
// contracts.
package parseeval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	MaxSnapshotJSONBytes = 64 << 10
	MaxResultJSONBytes   = 1 << 20
	MaxSummaryJSONBytes  = 1 << 20
	MaxJudgeRawBytes     = 64 << 10
)

type RunStatus string

const (
	RunDraft     RunStatus = "DRAFT"
	RunQueued    RunStatus = "QUEUED"
	RunRunning   RunStatus = "RUNNING"
	RunSucceeded RunStatus = "SUCCEEDED"
	RunPartial   RunStatus = "PARTIAL"
	RunFailed    RunStatus = "FAILED"
	RunCancelled RunStatus = "CANCELLED"
)

func (s RunStatus) Valid() bool {
	switch s {
	case RunDraft, RunQueued, RunRunning, RunSucceeded, RunPartial, RunFailed, RunCancelled:
		return true
	default:
		return false
	}
}

func (s RunStatus) Terminal() bool {
	return s == RunSucceeded || s == RunPartial || s == RunFailed || s == RunCancelled
}

type RunStage string

const (
	RunStageUploading   RunStage = "UPLOADING"
	RunStageValidating  RunStage = "VALIDATING"
	RunStageRendering   RunStage = "RENDERING"
	RunStageParsing     RunStage = "PARSING"
	RunStageScoring     RunStage = "SCORING"
	RunStageAggregating RunStage = "AGGREGATING"
)

func (s RunStage) Valid() bool {
	switch s {
	case RunStageUploading, RunStageValidating, RunStageRendering, RunStageParsing, RunStageScoring, RunStageAggregating:
		return true
	default:
		return false
	}
}

type DocumentStatus string

const (
	DocumentUploaded  DocumentStatus = "UPLOADED"
	DocumentRunning   DocumentStatus = "RUNNING"
	DocumentSucceeded DocumentStatus = "SUCCEEDED"
	DocumentPartial   DocumentStatus = "PARTIAL"
	DocumentFailed    DocumentStatus = "FAILED"
)

func (s DocumentStatus) Valid() bool {
	switch s {
	case DocumentUploaded, DocumentRunning, DocumentSucceeded, DocumentPartial, DocumentFailed:
		return true
	default:
		return false
	}
}

type DocumentStage string

const (
	DocumentStageValidating DocumentStage = "VALIDATING"
	DocumentStageRendering  DocumentStage = "RENDERING"
	DocumentStageParsing    DocumentStage = "PARSING"
	DocumentStageScoring    DocumentStage = "SCORING"
)

func (s DocumentStage) Valid() bool {
	switch s {
	case DocumentStageValidating, DocumentStageRendering, DocumentStageParsing, DocumentStageScoring:
		return true
	default:
		return false
	}
}

type StepStatus string

const (
	StepPending   StepStatus = "PENDING"
	StepSucceeded StepStatus = "SUCCEEDED"
	StepFailed    StepStatus = "FAILED"
	StepSkipped   StepStatus = "SKIPPED"
)

func (s StepStatus) Valid() bool {
	return s == StepPending || s == StepSucceeded || s == StepFailed || s == StepSkipped
}

type Engine string

const (
	EngineMarkItDown Engine = "markitdown"
	EngineAnyDoc     Engine = "anydoc"
)

func (e Engine) Valid() bool { return e == EngineMarkItDown || e == EngineAnyDoc }

type Format string

const (
	FormatDOCX Format = "docx"
	FormatPPTX Format = "pptx"
	FormatXLSX Format = "xlsx"
)

func (f Format) Valid() bool { return f == FormatDOCX || f == FormatPPTX || f == FormatXLSX }

type JudgeOrder string

const (
	JudgeMarkItDownA JudgeOrder = "markitdown-a"
	JudgeAnyDocA     JudgeOrder = "anydoc-a"
)

func (o JudgeOrder) Valid() bool { return o == JudgeMarkItDownA || o == JudgeAnyDocA }

type Winner string

const (
	WinnerMarkItDown Winner = "markitdown"
	WinnerAnyDoc     Winner = "anydoc"
	WinnerTie        Winner = "tie"
)

func (w Winner) Valid() bool { return w == WinnerMarkItDown || w == WinnerAnyDoc || w == WinnerTie }

type ArtifactKind string

const (
	ArtifactSource    ArtifactKind = "source"
	ArtifactTruthPage ArtifactKind = "truth-page"
	ArtifactMarkdown  ArtifactKind = "markdown"
	ArtifactJudgeRaw  ArtifactKind = "judge-raw"
)

func (k ArtifactKind) Valid() bool {
	return k == ArtifactSource || k == ArtifactTruthPage || k == ArtifactMarkdown || k == ArtifactJudgeRaw
}

type ErrorDetail struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func (e ErrorDetail) Validate() error {
	if !boundedText(e.Code, 128, false) || !boundedText(e.Message, 2048, false) {
		return errors.New("invalid bounded error detail")
	}
	return nil
}

type StoredArtifact struct {
	ObjectKey string `json:"objectKey"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
	ByteSize  int64  `json:"byteSize"`
}

func (a StoredArtifact) Validate() error {
	if !boundedText(a.ObjectKey, 1024, true) || strings.HasPrefix(a.ObjectKey, "/") || strings.Contains(a.ObjectKey, "\\") {
		return errors.New("invalid artifact object key")
	}
	if !canonicalSHA256(a.SHA256) || !boundedText(a.MediaType, 128, true) || a.ByteSize < 0 {
		return errors.New("invalid artifact identity")
	}
	return nil
}

type ParserDescriptor struct {
	Name           string `json:"name"`
	Version        string `json:"version"`
	WrapperVersion string `json:"wrapperVersion"`
}

func (d ParserDescriptor) Validate() error {
	if !boundedText(d.Name, 128, true) || !boundedText(d.Version, 128, true) || !boundedText(d.WrapperVersion, 128, true) {
		return errors.New("invalid parser descriptor")
	}
	return nil
}

type RendererDescriptor struct {
	ProtocolVersion    string `json:"protocolVersion"`
	ServiceVersion     string `json:"serviceVersion"`
	LibreOfficeVersion string `json:"libreOfficeVersion"`
	PyMuPDFVersion     string `json:"pyMuPDFVersion"`
}

func (d RendererDescriptor) Validate() error {
	if d.ProtocolVersion != "parser-eval-renderer/v1" || !boundedText(d.ServiceVersion, 128, true) ||
		!boundedText(d.LibreOfficeVersion, 128, true) || !boundedText(d.PyMuPDFVersion, 128, true) {
		return errors.New("invalid renderer descriptor")
	}
	return nil
}

type JudgeBindingSnapshot struct {
	ID                   string  `json:"id"`
	Provider             string  `json:"provider"`
	Model                string  `json:"model"`
	Fingerprint          string  `json:"fingerprint"`
	ModelDisplayName     string  `json:"modelDisplayName"`
	PricingKnown         bool    `json:"pricingKnown"`
	InputCostPerMillion  float64 `json:"inputCostPerMillion"`
	OutputCostPerMillion float64 `json:"outputCostPerMillion"`
}

func (s JudgeBindingSnapshot) Validate() error {
	if !boundedText(s.ID, 384, true) || !boundedText(s.Provider, 128, true) || !boundedText(s.Model, 255, true) ||
		!boundedText(s.ModelDisplayName, 255, true) || !canonicalSHA256(s.Fingerprint) {
		return errors.New("invalid judge binding identity")
	}
	if math.IsNaN(s.InputCostPerMillion) || math.IsInf(s.InputCostPerMillion, 0) || s.InputCostPerMillion < 0 ||
		math.IsNaN(s.OutputCostPerMillion) || math.IsInf(s.OutputCostPerMillion, 0) || s.OutputCostPerMillion < 0 {
		return errors.New("invalid judge binding prices")
	}
	return nil
}

type ExecutionSnapshot struct {
	MarkItDown         ParserDescriptor     `json:"markitdown"`
	AnyDoc             ParserDescriptor     `json:"anydoc"`
	Renderer           RendererDescriptor   `json:"renderer"`
	Judge              JudgeBindingSnapshot `json:"judge"`
	RenderDPI          int                  `json:"renderDPI"`
	MaxPages           int                  `json:"maxPages"`
	MarkdownJudgeChars int                  `json:"markdownJudgeChars"`
	JudgePromptVersion string               `json:"judgePromptVersion"`
	AppVersion         string               `json:"appVersion"`
	CreatedBy          string               `json:"createdBy"`
	ParserConcurrency  int                  `json:"parserConcurrency"`
}

func (s ExecutionSnapshot) Validate() error {
	if err := s.MarkItDown.Validate(); err != nil {
		return fmt.Errorf("markitdown descriptor: %w", err)
	}
	if err := s.AnyDoc.Validate(); err != nil {
		return fmt.Errorf("anydoc descriptor: %w", err)
	}
	if err := s.Renderer.Validate(); err != nil {
		return err
	}
	if err := s.Judge.Validate(); err != nil {
		return err
	}
	if s.RenderDPI <= 0 || s.RenderDPI > 300 || s.MaxPages <= 0 || s.MaxPages > 100 ||
		s.MarkdownJudgeChars <= 0 || s.MarkdownJudgeChars > 200_000 || s.ParserConcurrency != 1 {
		return errors.New("invalid execution snapshot limits")
	}
	if !boundedText(s.JudgePromptVersion, 128, true) || !boundedText(s.AppVersion, 256, true) || !boundedText(s.CreatedBy, 128, true) {
		return errors.New("invalid execution snapshot version or owner")
	}
	return nil
}

type RunProgress struct {
	DocumentsTotal     int    `json:"documentsTotal"`
	DocumentsCompleted int    `json:"documentsCompleted"`
	CurrentDocumentID  string `json:"currentDocumentId,omitempty"`
}

func (p RunProgress) Validate() error {
	if p.DocumentsTotal < 0 || p.DocumentsTotal > 50 || p.DocumentsCompleted < 0 || p.DocumentsCompleted > p.DocumentsTotal ||
		!boundedText(p.CurrentDocumentID, 128, false) {
		return errors.New("invalid run progress")
	}
	return nil
}

type TruthPage struct {
	Page     int            `json:"page"`
	Width    int            `json:"width"`
	Height   int            `json:"height"`
	Artifact StoredArtifact `json:"artifact"`
}

type TruthResult struct {
	Status           StepStatus         `json:"status"`
	Descriptor       RendererDescriptor `json:"descriptor"`
	TotalPages       int                `json:"totalPages"`
	CoveredPages     int                `json:"coveredPages"`
	Pages            []TruthPage        `json:"pages"`
	RenderDurationMS *int64             `json:"renderDurationMs,omitempty"`
	Error            ErrorDetail        `json:"error"`
}

func (r TruthResult) Successful() bool { return r.Status == StepSucceeded }

func (r TruthResult) Validate() error {
	if !r.Status.Valid() || len(r.Pages) > 100 || r.TotalPages < 0 || r.CoveredPages < 0 || r.CoveredPages > r.TotalPages || r.CoveredPages != len(r.Pages) {
		return errors.New("invalid truth result")
	}
	if r.RenderDurationMS != nil && *r.RenderDurationMS < 0 {
		return errors.New("negative render duration")
	}
	if err := r.Error.Validate(); err != nil {
		return err
	}
	if r.Status == StepSucceeded {
		if err := r.Descriptor.Validate(); err != nil {
			return err
		}
		for index, page := range r.Pages {
			if page.Page != index+1 || page.Width <= 0 || page.Height <= 0 {
				return errors.New("invalid truth page")
			}
			if err := page.Artifact.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

type StructureStats struct {
	Version             string `json:"version"`
	Characters          int    `json:"characters"`
	Headings            int    `json:"headings"`
	TableDataRows       int    `json:"tableDataRows"`
	ListItems           int    `json:"listItems"`
	Links               int    `json:"links"`
	ImageOrAssetMarkers int    `json:"imageOrAssetMarkers"`
	FootnoteDefinitions int    `json:"footnoteDefinitions"`
	ParserWarnings      int    `json:"parserWarnings"`
}

func (s StructureStats) Validate() error {
	if s.Version != "parser-structure-stats-v1" {
		return errors.New("invalid structure stats version")
	}
	for _, value := range []int{s.Characters, s.Headings, s.TableDataRows, s.ListItems, s.Links, s.ImageOrAssetMarkers, s.FootnoteDefinitions, s.ParserWarnings} {
		if value < 0 {
			return errors.New("negative structure stat")
		}
	}
	return nil
}

type ParseWarning struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Degraded bool   `json:"degraded"`
}

func (w ParseWarning) Validate() error {
	if !boundedText(w.Code, 128, true) || !boundedText(w.Message, 2048, true) {
		return errors.New("invalid parser warning")
	}
	return nil
}

type ParserResult struct {
	Status             StepStatus       `json:"status"`
	Descriptor         ParserDescriptor `json:"descriptor"`
	Order              int              `json:"order"`
	ParseDurationMS    *int64           `json:"parseDurationMs,omitempty"`
	EndToEndDurationMS *int64           `json:"endToEndDurationMs,omitempty"`
	Markdown           StoredArtifact   `json:"markdown"`
	Stats              StructureStats   `json:"stats"`
	Warnings           []ParseWarning   `json:"warnings"`
	Error              ErrorDetail      `json:"error"`
}

func (r ParserResult) Successful() bool { return r.Status == StepSucceeded }

func (r ParserResult) Validate() error {
	if !r.Status.Valid() || (r.Order != 0 && r.Order != 1 && r.Order != 2) || len(r.Warnings) > 1000 {
		return errors.New("invalid parser result")
	}
	for _, duration := range []*int64{r.ParseDurationMS, r.EndToEndDurationMS} {
		if duration != nil && *duration < 0 {
			return errors.New("negative parser duration")
		}
	}
	if err := r.Error.Validate(); err != nil {
		return err
	}
	if r.Status == StepSucceeded {
		if err := r.Descriptor.Validate(); err != nil {
			return err
		}
		if err := r.Markdown.Validate(); err != nil {
			return err
		}
		if err := r.Stats.Validate(); err != nil {
			return err
		}
		for _, warning := range r.Warnings {
			if err := warning.Validate(); err != nil {
				return err
			}
		}
	}
	return nil
}

type BlindScores struct {
	Completeness int `json:"completeness"`
	Structure    int `json:"structure"`
	Formatting   int `json:"formatting"`
	Cleanliness  int `json:"cleanliness"`
}

func (s BlindScores) Validate() error {
	for _, value := range []int{s.Completeness, s.Structure, s.Formatting, s.Cleanliness} {
		if value < 1 || value > 5 {
			return errors.New("judge score must be in [1,5]")
		}
	}
	return nil
}

type PositionWinner string

const (
	PositionWinnerA   PositionWinner = "A"
	PositionWinnerB   PositionWinner = "B"
	PositionWinnerTie PositionWinner = "tie"
)

func (w PositionWinner) Valid() bool {
	return w == PositionWinnerA || w == PositionWinnerB || w == PositionWinnerTie
}

type BlindVerdict struct {
	A      BlindScores    `json:"a"`
	B      BlindScores    `json:"b"`
	Winner PositionWinner `json:"winner"`
	Reason string         `json:"reason"`
}

func (v BlindVerdict) Validate() error {
	if err := v.A.Validate(); err != nil {
		return err
	}
	if err := v.B.Validate(); err != nil {
		return err
	}
	if !v.Winner.Valid() || !boundedText(v.Reason, 1024, true) {
		return errors.New("invalid judge winner or reason")
	}
	return nil
}

type TokenUsage struct {
	InputTokens         int64 `json:"inputTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	CacheReadTokens     int64 `json:"cacheReadTokens"`
	CacheCreationTokens int64 `json:"cacheCreationTokens"`
}

func (u TokenUsage) Validate() error {
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadTokens < 0 || u.CacheCreationTokens < 0 {
		return errors.New("negative token usage")
	}
	return nil
}

type JudgeSlot struct {
	Order            JudgeOrder     `json:"order"`
	Status           StepStatus     `json:"status"`
	Verdict          BlindVerdict   `json:"verdict"`
	Raw              StoredArtifact `json:"raw"`
	DurationMS       *int64         `json:"durationMs,omitempty"`
	Usage            TokenUsage     `json:"usage"`
	EstimatedCostUSD *float64       `json:"estimatedCostUsd,omitempty"`
	Error            ErrorDetail    `json:"error"`
}

func (s JudgeSlot) Successful() bool { return s.Status == StepSucceeded }

func (s JudgeSlot) Validate() error {
	if !s.Status.Valid() || (s.Order != "" && !s.Order.Valid()) {
		return errors.New("invalid judge slot")
	}
	if s.DurationMS != nil && *s.DurationMS < 0 {
		return errors.New("negative judge duration")
	}
	if s.EstimatedCostUSD != nil && (math.IsNaN(*s.EstimatedCostUSD) || math.IsInf(*s.EstimatedCostUSD, 0) || *s.EstimatedCostUSD < 0) {
		return errors.New("invalid judge cost")
	}
	if err := s.Usage.Validate(); err != nil {
		return err
	}
	if err := s.Error.Validate(); err != nil {
		return err
	}
	if s.Status == StepSucceeded {
		if !s.Order.Valid() {
			return errors.New("successful judge slot requires order")
		}
		if err := s.Verdict.Validate(); err != nil {
			return err
		}
		if err := s.Raw.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type QualityScore struct {
	Completeness float64 `json:"completeness"`
	Structure    float64 `json:"structure"`
	Formatting   float64 `json:"formatting"`
	Cleanliness  float64 `json:"cleanliness"`
	Total        float64 `json:"total"`
}

func (s QualityScore) Validate() error {
	for _, value := range []float64{s.Completeness, s.Structure, s.Formatting, s.Cleanliness, s.Total} {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 100 {
			return errors.New("quality score must be in [0,100]")
		}
	}
	return nil
}

type JudgeResult struct {
	Status           StepStatus    `json:"status"`
	MarkItDownA      JudgeSlot     `json:"markitdownA"`
	AnyDocA          JudgeSlot     `json:"anydocA"`
	MarkItDownScore  *QualityScore `json:"markitdownScore,omitempty"`
	AnyDocScore      *QualityScore `json:"anydocScore,omitempty"`
	Winner           *Winner       `json:"winner,omitempty"`
	Usage            TokenUsage    `json:"usage"`
	EstimatedCostUSD *float64      `json:"estimatedCostUsd,omitempty"`
	Error            ErrorDetail   `json:"error"`
}

func (r JudgeResult) Validate() error {
	if !r.Status.Valid() {
		return errors.New("invalid judge result status")
	}
	if err := r.MarkItDownA.Validate(); err != nil {
		return err
	}
	if err := r.AnyDocA.Validate(); err != nil {
		return err
	}
	if err := r.Usage.Validate(); err != nil {
		return err
	}
	if err := r.Error.Validate(); err != nil {
		return err
	}
	if r.Status == StepSucceeded {
		if r.MarkItDownScore == nil || r.AnyDocScore == nil || r.Winner == nil || !r.Winner.Valid() {
			return errors.New("successful judge result requires both scores and winner")
		}
		if err := r.MarkItDownScore.Validate(); err != nil {
			return err
		}
		if err := r.AnyDocScore.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type DurationAggregate struct {
	Samples int      `json:"samples"`
	Median  *float64 `json:"medianMs,omitempty"`
	P95     *float64 `json:"p95Ms,omitempty"`
}

type ParserAggregate struct {
	Documents   int               `json:"documents"`
	Successes   int               `json:"successes"`
	SuccessRate float64           `json:"successRate"`
	Parse       DurationAggregate `json:"parse"`
	EndToEnd    DurationAggregate `json:"endToEnd"`
	Quality     *QualityScore     `json:"quality,omitempty"`
}

type QualityComparison struct {
	MarkItDown QualityScore `json:"markitdown"`
	AnyDoc     QualityScore `json:"anydoc"`
}

type FormatAggregate struct {
	Format     Format             `json:"format"`
	Documents  int                `json:"documents"`
	MarkItDown ParserAggregate    `json:"markitdown"`
	AnyDoc     ParserAggregate    `json:"anydoc"`
	Quality    *QualityComparison `json:"quality,omitempty"`
}

type WinCounts struct {
	MarkItDown int `json:"markitdown"`
	AnyDoc     int `json:"anydoc"`
	Ties       int `json:"ties"`
	Failures   int `json:"failures"`
}

type RunSummary struct {
	MarkItDown       ParserAggregate    `json:"markitdown"`
	AnyDoc           ParserAggregate    `json:"anydoc"`
	Formats          []FormatAggregate  `json:"formats"`
	MacroQuality     *QualityComparison `json:"macroQuality,omitempty"`
	Wins             WinCounts          `json:"wins"`
	Usage            TokenUsage         `json:"usage"`
	EstimatedCostUSD *float64           `json:"estimatedCostUsd,omitempty"`
}

func (s RunSummary) Validate() error {
	if len(s.Formats) > 3 {
		return errors.New("too many format aggregates")
	}
	if err := s.Usage.Validate(); err != nil {
		return err
	}
	if s.EstimatedCostUSD != nil && (math.IsNaN(*s.EstimatedCostUSD) || math.IsInf(*s.EstimatedCostUSD, 0) || *s.EstimatedCostUSD < 0) {
		return errors.New("invalid summary cost")
	}
	return nil
}

func DecodeClosedJSON[T any](raw []byte, maxBytes int, validate func(*T) error) (T, error) {
	var zero T
	if maxBytes <= 0 || len(raw) > maxBytes {
		return zero, errors.New("JSON exceeds byte limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var value T
	if err := decoder.Decode(&value); err != nil {
		return zero, fmt.Errorf("decode closed JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return zero, errors.New("JSON contains trailing data")
	}
	if validate != nil {
		if err := validate(&value); err != nil {
			return zero, err
		}
	}
	return value, nil
}

func EncodeBoundedJSON(value any, maxBytes int) (string, error) {
	if validator, ok := value.(interface{ Validate() error }); ok {
		if err := validator.Validate(); err != nil {
			return "", err
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if maxBytes <= 0 || len(encoded) > maxBytes {
		return "", errors.New("encoded JSON exceeds byte limit")
	}
	return string(encoded), nil
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

func canonicalSHA256(value string) bool { return sha256Pattern.MatchString(value) }

func boundedText(value string, maxRunes int, required bool) bool {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return false
	}
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return false
	}
	return utf8.RuneCountInString(value) <= maxRunes
}
