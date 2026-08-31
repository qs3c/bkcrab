package setup

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/buildinfo"
	"github.com/qs3c/bkcrab/internal/parseeval"
	"github.com/qs3c/bkcrab/internal/store"
)

func (s *Server) registerParserEvaluationRoutes(mux *http.ServeMux, gate func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/admin/parser-evals/capabilities", gate(s.handleParserEvalCapabilities))
	mux.HandleFunc("GET /api/admin/parser-evals/runs", gate(s.handleListParserEvalRuns))
	mux.HandleFunc("POST /api/admin/parser-evals/runs", gate(s.handleCreateParserEvalRun))
	mux.HandleFunc("GET /api/admin/parser-evals/runs/{runId}", gate(s.handleGetParserEvalRun))
	mux.HandleFunc("DELETE /api/admin/parser-evals/runs/{runId}", gate(s.handleDeleteParserEvalRun))
	mux.HandleFunc("POST /api/admin/parser-evals/runs/{runId}/documents", gate(s.handleUploadParserEvalDocument))
	mux.HandleFunc("DELETE /api/admin/parser-evals/runs/{runId}/documents/{documentId}", gate(s.handleRemoveParserEvalDocument))
	mux.HandleFunc("POST /api/admin/parser-evals/runs/{runId}/start", gate(s.handleStartParserEvalRun))
	mux.HandleFunc("POST /api/admin/parser-evals/runs/{runId}/cancel", gate(s.handleCancelParserEvalRun))
	mux.HandleFunc("POST /api/admin/parser-evals/runs/{runId}/retry", gate(s.handleRetryParserEvalRun))
	mux.HandleFunc("GET /api/admin/parser-evals/runs/{runId}/documents/{documentId}/artifacts/{kind}", gate(s.handleParserEvalArtifact))
}

func (s *Server) parserEvalServiceOrError(w http.ResponseWriter) (*parseeval.Service, bool) {
	if s.parserEvalService == nil {
		writeEvalError(w, http.StatusServiceUnavailable, "parser_eval_unavailable", "document parser evaluation is unavailable")
		return nil, false
	}
	return s.parserEvalService, true
}

func (s *Server) parserEvalCapabilities(r *http.Request) (parseeval.Capabilities, error) {
	if s.parserEvalCaps == nil {
		return parseeval.Capabilities{}, errors.New("parser evaluation capabilities are unavailable")
	}
	return s.parserEvalCaps.ParserEvaluationCapabilities(r.Context(), evalIdentity(r))
}

func (s *Server) handleParserEvalCapabilities(w http.ResponseWriter, r *http.Request) {
	capabilities, err := s.parserEvalCapabilities(r)
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "data": capabilities})
}

func (s *Server) handleListParserEvalRuns(w http.ResponseWriter, r *http.Request) {
	service, ok := s.parserEvalServiceOrError(w)
	if !ok {
		return
	}
	cursor, limit := evalListParams(r)
	if limit > 50 {
		limit = 50
	}
	runs, err := service.ListRuns(r.Context(), evalIdentity(r), cursor, limit)
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	items := make([]parserEvalRunDTO, 0, len(runs))
	for index := range runs {
		items = append(items, newParserEvalRunDTO(runs[index]))
	}
	nextCursor := ""
	if len(runs) == limit {
		nextCursor = runs[len(runs)-1].ID
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "data": map[string]any{"items": items, "nextCursor": nextCursor}})
}

func (s *Server) handleCreateParserEvalRun(w http.ResponseWriter, r *http.Request) {
	service, ok := s.parserEvalServiceOrError(w)
	if !ok {
		return
	}
	key, valid := evalIdempotencyKey(r)
	if !valid {
		writeEvalError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 characters")
		return
	}
	var request struct {
		JudgeModelBindingID string `json:"judgeModelBindingId"`
	}
	if !decodeEvalJSON(w, r, 64<<10, &request) {
		return
	}
	capabilities, err := s.parserEvalCapabilities(r)
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	selected, found := findParserEvalJudgeBinding(capabilities, strings.TrimSpace(request.JudgeModelBindingID))
	if !capabilities.Enabled || !found {
		writeEvalError(w, http.StatusUnprocessableEntity, "invalid_judge_model", "select an available vision judge model")
		return
	}
	run, err := service.CreateDraft(r.Context(), parseeval.CreateDraftRequest{
		CreatedBy:           evalIdentity(r),
		JudgeModelBindingID: selected.ID,
		IdempotencyKey:      key,
	})
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]any{"ok": true, "data": newParserEvalRunDTO(*run)})
}

func (s *Server) handleGetParserEvalRun(w http.ResponseWriter, r *http.Request) {
	service, ok := s.parserEvalServiceOrError(w)
	if !ok {
		return
	}
	detail, err := service.GetRun(r.Context(), r.PathValue("runId"), evalIdentity(r))
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "data": newParserEvalRunDetailDTO(*detail)})
}

func (s *Server) handleUploadParserEvalDocument(w http.ResponseWriter, r *http.Request) {
	service, ok := s.parserEvalServiceOrError(w)
	if !ok {
		return
	}
	key, valid := evalIdempotencyKey(r)
	if !valid {
		writeEvalError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 characters")
		return
	}
	maxBody := service.Limits().MaxFileBytes + (1 << 20)
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeEvalError(w, http.StatusRequestEntityTooLarge, "file_too_large", "uploaded document exceeds the configured size limit")
		} else {
			writeEvalError(w, http.StatusBadRequest, "invalid_multipart", "request must contain exactly one file field named file")
		}
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if r.MultipartForm == nil || len(r.MultipartForm.Value) != 0 || len(r.MultipartForm.File) != 1 || len(r.MultipartForm.File["file"]) != 1 {
		writeEvalError(w, http.StatusBadRequest, "invalid_multipart", "request must contain exactly one file field named file")
		return
	}
	header := r.MultipartForm.File["file"][0]
	file, err := header.Open()
	if err != nil {
		writeEvalError(w, http.StatusBadRequest, "invalid_multipart", "uploaded document could not be opened")
		return
	}
	defer file.Close()
	document, err := service.UploadDocument(r.Context(), parseeval.UploadDocumentRequest{
		RunID:             r.PathValue("runId"),
		CreatedBy:         evalIdentity(r),
		FileName:          header.Filename,
		MediaType:         header.Header.Get("Content-Type"),
		DeclaredSizeBytes: header.Size,
		Reader:            file,
		IdempotencyKey:    key,
	})
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]any{"ok": true, "data": newParserEvalDocumentDTO(*document)})
}

func (s *Server) handleRemoveParserEvalDocument(w http.ResponseWriter, r *http.Request) {
	service, ok := s.parserEvalServiceOrError(w)
	if !ok {
		return
	}
	if err := service.RemoveDraftDocument(r.Context(), r.PathValue("runId"), r.PathValue("documentId"), evalIdentity(r)); err != nil {
		writeParserEvalError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStartParserEvalRun(w http.ResponseWriter, r *http.Request) {
	service, ok := s.parserEvalServiceOrError(w)
	if !ok {
		return
	}
	if _, valid := evalIdempotencyKey(r); !valid {
		writeEvalError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 characters")
		return
	}
	if !requireEmptyParserEvalBody(w, r) {
		return
	}
	detail, err := service.GetRun(r.Context(), r.PathValue("runId"), evalIdentity(r))
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	if detail.Run.Status == store.ParserEvalRunQueued || detail.Run.Status == store.ParserEvalRunRunning {
		jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "data": newParserEvalRunDTO(detail.Run)})
		return
	}
	selection, err := parseeval.DecodeClosedJSON[parseeval.DraftSelection]([]byte(detail.Run.ExecutionSnapshotJSON), parseeval.MaxSnapshotJSONBytes, func(value *parseeval.DraftSelection) error { return value.Validate() })
	if err != nil {
		writeEvalError(w, http.StatusConflict, "run_not_draft", "run is not a startable draft")
		return
	}
	capabilities, err := s.parserEvalCapabilities(r)
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	judge, found := findParserEvalJudgeBinding(capabilities, selection.JudgeModelBindingID)
	markitdown, markitdownFound := findParserEvalParser(capabilities, parseeval.EngineMarkItDown)
	anydoc, anydocFound := findParserEvalParser(capabilities, parseeval.EngineAnyDoc)
	if !capabilities.Available || !found || !markitdownFound || !anydocFound {
		writeEvalError(w, http.StatusServiceUnavailable, "dependencies_unavailable", "parser evaluation dependencies or selected judge model are unavailable")
		return
	}
	snapshot := parseeval.ExecutionSnapshot{
		MarkItDown:         markitdown.Descriptor,
		AnyDoc:             anydoc.Descriptor,
		Renderer:           capabilities.Renderer.Descriptor,
		Judge:              judge,
		RenderDPI:          capabilities.RenderDPI,
		MaxPages:           capabilities.MaxPages,
		MarkdownJudgeChars: capabilities.MarkdownJudgeChars,
		JudgePromptVersion: parseeval.JudgePromptVersion,
		AppVersion:         buildinfo.Version,
		CreatedBy:          evalIdentity(r),
		ParserConcurrency:  1,
	}
	run, err := service.StartRun(r.Context(), detail.Run.ID, evalIdentity(r), snapshot)
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "data": newParserEvalRunDTO(*run)})
}

func (s *Server) handleCancelParserEvalRun(w http.ResponseWriter, r *http.Request) {
	s.handleParserEvalRunAction(w, r, "cancel")
}

func (s *Server) handleRetryParserEvalRun(w http.ResponseWriter, r *http.Request) {
	s.handleParserEvalRunAction(w, r, "retry")
}

func (s *Server) handleParserEvalRunAction(w http.ResponseWriter, r *http.Request, action string) {
	service, ok := s.parserEvalServiceOrError(w)
	if !ok {
		return
	}
	if _, valid := evalIdempotencyKey(r); !valid {
		writeEvalError(w, http.StatusBadRequest, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 characters")
		return
	}
	if !requireEmptyParserEvalBody(w, r) {
		return
	}
	var run *store.ParserEvalRunRecord
	var err error
	if action == "cancel" {
		run, err = service.CancelRun(r.Context(), r.PathValue("runId"), evalIdentity(r))
	} else {
		run, err = service.RetryRun(r.Context(), r.PathValue("runId"), evalIdentity(r))
	}
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "data": newParserEvalRunDTO(*run)})
}

func (s *Server) handleDeleteParserEvalRun(w http.ResponseWriter, r *http.Request) {
	service, ok := s.parserEvalServiceOrError(w)
	if !ok {
		return
	}
	if s.parserEvalCleanup == nil {
		writeEvalError(w, http.StatusServiceUnavailable, "cleanup_unavailable", "parser evaluation cleanup is unavailable")
		return
	}
	detail, err := service.GetRun(r.Context(), r.PathValue("runId"), evalIdentity(r))
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	if detail.Run.Status == store.ParserEvalRunDraft {
		if _, err = service.CancelRun(r.Context(), detail.Run.ID, evalIdentity(r)); err != nil {
			writeParserEvalError(w, err)
			return
		}
	} else if !parseeval.RunStatus(detail.Run.Status).Terminal() {
		writeEvalError(w, http.StatusConflict, "run_active", "cancel the active run and wait for it to become terminal before deleting it")
		return
	}
	if err = s.parserEvalCleanup.DeleteRun(r.Context(), detail.Run.ID); err != nil {
		writeParserEvalError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleParserEvalArtifact(w http.ResponseWriter, r *http.Request) {
	service, ok := s.parserEvalServiceOrError(w)
	if !ok {
		return
	}
	if _, err := service.GetRun(r.Context(), r.PathValue("runId"), evalIdentity(r)); err != nil {
		writeParserEvalError(w, err)
		return
	}
	request, err := parserEvalArtifactRequest(r, service.Limits().MaxPages)
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	artifact, err := service.OpenArtifact(r.Context(), r.PathValue("runId"), r.PathValue("documentId"), request)
	if err != nil {
		writeParserEvalError(w, err)
		return
	}
	defer artifact.Reader.Close()
	w.Header().Set("Content-Type", artifact.ContentType)
	w.Header().Set("Content-Length", strconv.FormatInt(artifact.SizeBytes, 10))
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": artifact.FileName}))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store, private")
	w.WriteHeader(http.StatusOK)
	if _, err := io.CopyN(w, artifact.Reader, artifact.SizeBytes); err != nil && !errors.Is(err, io.EOF) {
		slog.Warn("stream parser evaluation artifact", "error", err)
	}
}

func parserEvalArtifactRequest(r *http.Request, maxPages int) (parseeval.ArtifactRequest, error) {
	request := parseeval.ArtifactRequest{Kind: parseeval.ArtifactKind(r.PathValue("kind"))}
	query := r.URL.Query()
	exact := func(name string) (string, bool) {
		values, exists := query[name]
		returnValue := ""
		if exists && len(values) == 1 {
			returnValue = values[0]
		}
		return returnValue, exists && len(values) == 1
	}
	switch request.Kind {
	case parseeval.ArtifactSource:
		if len(query) != 0 {
			return request, parseeval.ErrInvalidArtifactRequest
		}
	case parseeval.ArtifactTruthPage:
		value, valid := exact("page")
		page, parseErr := strconv.Atoi(value)
		if len(query) != 1 || !valid || parseErr != nil || page < 1 || page > maxPages {
			return request, parseeval.ErrInvalidArtifactRequest
		}
		request.Page = page
	case parseeval.ArtifactMarkdown:
		value, valid := exact("engine")
		request.Engine = parseeval.Engine(value)
		if len(query) != 1 || !valid || !request.Engine.Valid() {
			return request, parseeval.ErrInvalidArtifactRequest
		}
	case parseeval.ArtifactJudgeRaw:
		value, valid := exact("order")
		request.Order = parseeval.JudgeOrder(value)
		if len(query) != 1 || !valid || !request.Order.Valid() {
			return request, parseeval.ErrInvalidArtifactRequest
		}
	default:
		return request, parseeval.ErrInvalidArtifactRequest
	}
	return request, nil
}

func findParserEvalJudgeBinding(capabilities parseeval.Capabilities, id string) (parseeval.JudgeBindingSnapshot, bool) {
	for _, binding := range capabilities.JudgeModelBindings {
		if binding.ID == id {
			return binding, true
		}
	}
	return parseeval.JudgeBindingSnapshot{}, false
}

func findParserEvalParser(capabilities parseeval.Capabilities, engine parseeval.Engine) (parseeval.ParserCapability, bool) {
	for _, parser := range capabilities.Parsers {
		if parser.Engine == engine && parser.Health.Healthy {
			return parser, true
		}
	}
	return parseeval.ParserCapability{}, false
}

func requireEmptyParserEvalBody(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1)
	content, err := io.ReadAll(r.Body)
	if err != nil || len(content) != 0 {
		writeEvalError(w, http.StatusBadRequest, "invalid_request", "request body must be empty")
		return false
	}
	return true
}

func writeParserEvalError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeEvalError(w, http.StatusNotFound, "not_found", "parser evaluation resource not found")
	case errors.Is(err, parseeval.ErrForbidden):
		writeEvalError(w, http.StatusForbidden, "forbidden", "parser evaluation resource is not accessible")
	case errors.Is(err, parseeval.ErrFileTooLarge):
		writeEvalError(w, http.StatusRequestEntityTooLarge, "file_too_large", "uploaded document exceeds the configured size limit")
	case errors.Is(err, parseeval.ErrIdempotencyConflict):
		writeEvalError(w, http.StatusConflict, "idempotency_conflict", "Idempotency-Key was already used for a different request")
	case errors.Is(err, parseeval.ErrUploadInProgress):
		writeEvalError(w, http.StatusConflict, "upload_in_progress", "an upload with this Idempotency-Key is still in progress")
	case errors.Is(err, parseeval.ErrImmutable), errors.Is(err, store.ErrParserEvalImmutable), errors.Is(err, store.ErrParserEvalActive):
		writeEvalError(w, http.StatusConflict, "invalid_run_state", "parser evaluation run is not in a compatible state")
	case errors.Is(err, store.ErrParserEvalLimit):
		writeEvalError(w, http.StatusUnprocessableEntity, "upload_limit_exceeded", "document count or total byte limit would be exceeded")
	case errors.Is(err, store.ErrParserEvalIncomplete):
		writeEvalError(w, http.StatusUnprocessableEntity, "documents_incomplete", "at least one fully uploaded document is required")
	case errors.Is(err, parseeval.ErrInvalidArtifactRequest):
		writeEvalError(w, http.StatusBadRequest, "invalid_artifact_request", "artifact parameters are invalid")
	case errors.Is(err, parseeval.ErrArtifactUnavailable):
		writeEvalError(w, http.StatusNotFound, "artifact_unavailable", "requested parser evaluation artifact is unavailable")
	case errors.Is(err, parseeval.ErrInvalidUpload), errors.Is(err, parseeval.ErrDeclaredSizeMismatch):
		writeEvalError(w, http.StatusUnprocessableEntity, "invalid_document", "uploaded document is invalid")
	default:
		slog.Error("parser evaluation API failed", "error", err)
		writeEvalError(w, http.StatusInternalServerError, "parser_eval_failed", "parser evaluation request could not be completed")
	}
}

type parserEvalRunDTO struct {
	ID                string     `json:"id"`
	Status            string     `json:"status"`
	Stage             string     `json:"stage"`
	Progress          any        `json:"progress"`
	ExecutionSnapshot any        `json:"executionSnapshot"`
	Summary           any        `json:"summary"`
	CreatedBy         string     `json:"createdBy"`
	ErrorCode         string     `json:"errorCode,omitempty"`
	ErrorMessage      string     `json:"errorMessage,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
	StartedAt         *time.Time `json:"startedAt,omitempty"`
	FinishedAt        *time.Time `json:"finishedAt,omitempty"`
	ExpiresAt         time.Time  `json:"expiresAt"`
	CancelRequestedAt *time.Time `json:"cancelRequestedAt,omitempty"`
}

type parserEvalDocumentDTO struct {
	ID           string    `json:"id"`
	RunID        string    `json:"runId"`
	Ordinal      int64     `json:"ordinal"`
	FileName     string    `json:"fileName"`
	Format       string    `json:"format"`
	MediaType    string    `json:"mediaType"`
	SizeBytes    int64     `json:"sizeBytes"`
	SHA256       string    `json:"sha256"`
	Status       string    `json:"status"`
	Stage        string    `json:"stage"`
	Truth        any       `json:"truth"`
	MarkItDown   any       `json:"markitdown"`
	AnyDoc       any       `json:"anydoc"`
	Judge        any       `json:"judge"`
	ErrorCode    string    `json:"errorCode,omitempty"`
	ErrorMessage string    `json:"errorMessage,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type parserEvalRunDetailDTO struct {
	Run       parserEvalRunDTO        `json:"run"`
	Documents []parserEvalDocumentDTO `json:"documents"`
	Summary   any                     `json:"summary"`
}

func newParserEvalRunDTO(record store.ParserEvalRunRecord) parserEvalRunDTO {
	return parserEvalRunDTO{
		ID: record.ID, Status: record.Status, Stage: record.Stage,
		Progress: decodePublicParserEvalJSON(record.ProgressJSON), ExecutionSnapshot: decodePublicParserEvalJSON(record.ExecutionSnapshotJSON),
		Summary: decodePublicParserEvalJSON(record.SummaryJSON), CreatedBy: record.CreatedBy,
		ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
		StartedAt: nullableParserEvalTime(record.StartedAt.Valid, record.StartedAt.Time), FinishedAt: nullableParserEvalTime(record.FinishedAt.Valid, record.FinishedAt.Time),
		ExpiresAt: record.ExpiresAt, CancelRequestedAt: nullableParserEvalTime(record.CancelRequestedAt.Valid, record.CancelRequestedAt.Time),
	}
}

func newParserEvalDocumentDTO(record store.ParserEvalDocumentRecord) parserEvalDocumentDTO {
	return parserEvalDocumentDTO{
		ID: record.ID, RunID: record.RunID, Ordinal: record.Ordinal, FileName: record.FileName, Format: record.Format,
		MediaType: record.MediaType, SizeBytes: record.SizeBytes, SHA256: record.SHA256, Status: record.Status, Stage: record.Stage,
		Truth: decodePublicParserEvalJSON(record.TruthJSON), MarkItDown: decodePublicParserEvalJSON(record.MarkItDownResultJSON),
		AnyDoc: decodePublicParserEvalJSON(record.AnyDocResultJSON), Judge: decodePublicParserEvalJSON(record.JudgeResultJSON),
		ErrorCode: record.ErrorCode, ErrorMessage: record.ErrorMessage, CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}
}

func newParserEvalRunDetailDTO(detail parseeval.RunWithDocuments) parserEvalRunDetailDTO {
	documents := make([]parserEvalDocumentDTO, 0, len(detail.Documents))
	for index := range detail.Documents {
		documents = append(documents, newParserEvalDocumentDTO(detail.Documents[index]))
	}
	return parserEvalRunDetailDTO{Run: newParserEvalRunDTO(detail.Run), Documents: documents, Summary: decodePublicParserEvalJSON(detail.Run.SummaryJSON)}
}

func decodePublicParserEvalJSON(raw string) any {
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil
	}
	return scrubParserEvalObjectKeys(value)
}

func scrubParserEvalObjectKeys(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		delete(typed, "objectKey")
		for key, child := range typed {
			typed[key] = scrubParserEvalObjectKeys(child)
		}
	case []any:
		for index := range typed {
			typed[index] = scrubParserEvalObjectKeys(typed[index])
		}
	}
	return value
}

func nullableParserEvalTime(valid bool, value time.Time) *time.Time {
	if !valid {
		return nil
	}
	return &value
}
