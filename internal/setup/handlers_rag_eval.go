package setup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/store"
)

func (s *Server) registerRAGEvaluationRoutes(mux *http.ServeMux, gate func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/admin/rag-evals/capabilities", gate(s.handleRAGEvalCapabilities))
	mux.HandleFunc("GET /api/admin/rag-evals/catalog", gate(s.handleRAGEvalCatalog))
	mux.HandleFunc("GET /api/admin/rag-evals/catalog-imports", gate(s.handleListRAGEvalCatalogImports))
	mux.HandleFunc("POST /api/admin/rag-evals/catalog-imports", gate(s.handleCreateRAGEvalCatalogImport))
	mux.HandleFunc("GET /api/admin/rag-evals/catalog-imports/{id}", gate(s.handleGetRAGEvalCatalogImport))
	mux.HandleFunc("POST /api/admin/rag-evals/catalog-imports/{id}/cancel", gate(s.handleCancelRAGEvalCatalogImport))
	mux.HandleFunc("GET /api/admin/rag-evals/datasets", gate(s.handleListRAGEvalDatasets))
	mux.HandleFunc("POST /api/admin/rag-evals/datasets", gate(s.handleCreateRAGEvalDataset))
	mux.HandleFunc("GET /api/admin/rag-evals/datasets/{id}", gate(s.handleGetRAGEvalDataset))
	mux.HandleFunc("DELETE /api/admin/rag-evals/datasets/{id}", gate(s.handleDeleteRAGEvalDataset))
	mux.HandleFunc("POST /api/admin/rag-evals/datasets/{id}/versions", gate(s.handleCreateRAGEvalDatasetVersion))
	mux.HandleFunc("POST /api/admin/rag-evals/dataset-versions/{id}/validate", gate(s.handleValidateRAGEvalDatasetVersion))
	mux.HandleFunc("GET /api/admin/rag-evals/dataset-versions/{id}/validation", gate(s.handleRAGEvalDatasetValidation))
	mux.HandleFunc("GET /api/admin/rag-evals/profiles", gate(s.handleListRAGEvalProfiles))
	mux.HandleFunc("POST /api/admin/rag-evals/profiles", gate(s.handleCreateRAGEvalProfile))
	mux.HandleFunc("GET /api/admin/rag-evals/runs", gate(s.handleListRAGEvalRuns))
	mux.HandleFunc("POST /api/admin/rag-evals/runs", gate(s.handleCreateRAGEvalRun))
	mux.HandleFunc("GET /api/admin/rag-evals/runs/{id}", gate(s.handleGetRAGEvalRun))
	mux.HandleFunc("DELETE /api/admin/rag-evals/runs/{id}", gate(s.handleDeleteRAGEvalRun))
	mux.HandleFunc("POST /api/admin/rag-evals/runs/{id}/cancel", gate(s.handleCancelRAGEvalRun))
	mux.HandleFunc("GET /api/admin/rag-evals/runs/{id}/cases", gate(s.handleListRAGEvalRunCases))
	mux.HandleFunc("GET /api/admin/rag-evals/runs/{id}/compare/{baselineId}", gate(s.handleCompareRAGEvalRuns))
	mux.HandleFunc("GET /api/admin/rag-evals/runs/{id}/export", gate(s.handleExportRAGEvalRun))
	mux.HandleFunc("GET /api/admin/rag-policies", gate(s.handleListRAGPolicies))
}

func (s *Server) evalService(w http.ResponseWriter) (*eval.AdminService, bool) {
	value := s.ragEvalAdmin
	if value == nil {
		writeEvalError(w, http.StatusServiceUnavailable, "eval_unavailable", "RAG evaluation service is unavailable")
		return nil, false
	}
	return value, true
}

func writeEvalError(w http.ResponseWriter, status int, code, message string) {
	jsonResponse(w, status, map[string]any{"ok": false, "error": map[string]string{"code": code, "message": message}})
}

func writeEvalServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeEvalError(w, http.StatusNotFound, "not_found", "evaluation resource not found")
	case errors.Is(err, eval.ErrDatasetValidation):
		writeEvalError(w, http.StatusUnprocessableEntity, "dataset_validation_failed", "dataset validation failed")
	default:
		writeEvalError(w, http.StatusBadRequest, "invalid_request", err.Error())
	}
}

func evalListParams(r *http.Request) (string, int) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return strings.TrimSpace(r.URL.Query().Get("cursor")), limit
}

func evalIdentity(r *http.Request) string {
	identity, _ := auth.FromContext(r.Context())
	return identity.UserID
}

func decodeEvalJSON(w http.ResponseWriter, r *http.Request, max int64, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeEvalError(w, http.StatusBadRequest, "invalid_json", "request body is invalid")
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		writeEvalError(w, http.StatusBadRequest, "invalid_json", "request body contains trailing JSON")
		return false
	}
	return true
}

func evalIdempotencyKey(r *http.Request) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	return key, key == "" || (len(key) >= 8 && len(key) <= 128)
}

func evalDeterministicID(prefix, actor, key string) string {
	digest := sha256.Sum256([]byte(actor + "\x00" + key))
	return prefix + hex.EncodeToString(digest[:16])
}

func (s *Server) handleRAGEvalCapabilities(w http.ResponseWriter, _ *http.Request) {
	snapshot := s.ragEvaluatorHealthSnapshot()
	healthy := snapshot.Healthy && snapshot.Fresh(time.Now())
	reason := snapshot.Reason
	if reason == "" && !healthy {
		reason = "evaluator health has not been observed"
	}
	if !s.ragCfg.Evaluation.Enabled {
		reason = "RAG evaluation is disabled"
	}
	jsonResponse(w, http.StatusOK, s.ragCfg.Evaluation.Capabilities(healthy, reason))
}

func (s *Server) handleRAGEvalCatalog(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]any{"items": eval.BuiltinCatalog()})
}

func (s *Server) evalCatalogImporter(w http.ResponseWriter) (*eval.CatalogImportRunner, bool) {
	if s.ragEvalCatalog == nil {
		writeEvalError(w, http.StatusServiceUnavailable, "catalog_import_unavailable", "evaluation catalog import service is unavailable")
		return nil, false
	}
	return s.ragEvalCatalog, true
}

func (s *Server) handleCreateRAGEvalCatalogImport(w http.ResponseWriter, r *http.Request) {
	if !s.ragCfg.Evaluation.Enabled {
		writeEvalError(w, http.StatusServiceUnavailable, "eval_disabled", "RAG evaluation is disabled")
		return
	}
	service, ok := s.evalCatalogImporter(w)
	if !ok {
		return
	}
	var request struct {
		DatasetID string `json:"datasetId"`
		eval.CatalogImportOptions
	}
	if !decodeEvalJSON(w, r, 64<<10, &request) {
		return
	}
	record, err := service.Create(r.Context(), strings.TrimSpace(request.DatasetID), evalIdentity(r), request.CatalogImportOptions)
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, record)
}

func (s *Server) handleListRAGEvalCatalogImports(w http.ResponseWriter, r *http.Request) {
	service, ok := s.evalCatalogImporter(w)
	if !ok {
		return
	}
	cursor, limit := evalListParams(r)
	items, err := service.List(r.Context(), cursor, limit)
	if err != nil {
		writeEvalError(w, http.StatusInternalServerError, "list_failed", "could not list catalog imports")
		return
	}
	next := ""
	if len(items) == limit {
		next = items[len(items)-1].ID
	}
	jsonResponse(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (s *Server) handleGetRAGEvalCatalogImport(w http.ResponseWriter, r *http.Request) {
	service, ok := s.evalCatalogImporter(w)
	if !ok {
		return
	}
	record, err := service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, record)
}

func (s *Server) handleCancelRAGEvalCatalogImport(w http.ResponseWriter, r *http.Request) {
	service, ok := s.evalCatalogImporter(w)
	if !ok {
		return
	}
	changed, err := service.Cancel(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	if !changed {
		writeEvalError(w, http.StatusConflict, "not_cancellable", "catalog import is already terminal or cancellation was already requested")
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]any{"ok": true})
}

func (s *Server) handleListRAGEvalDatasets(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	cursor, limit := evalListParams(r)
	items, err := st.ListDatasets(r.Context(), cursor, limit)
	if err != nil {
		writeEvalError(w, 500, "list_failed", "could not list datasets")
		return
	}
	next := ""
	if len(items) == limit {
		next = items[len(items)-1].ID
	}
	jsonResponse(w, 200, map[string]any{"items": items, "nextCursor": next})
}

func (s *Server) handleCreateRAGEvalDataset(w http.ResponseWriter, r *http.Request) {
	if !s.ragCfg.Evaluation.Enabled {
		writeEvalError(w, 503, "eval_disabled", "RAG evaluation is disabled")
		return
	}
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	key, valid := evalIdempotencyKey(r)
	if !valid {
		writeEvalError(w, 400, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 characters")
		return
	}
	var request struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !decodeEvalJSON(w, r, 64<<10, &request) {
		return
	}
	if strings.TrimSpace(request.Name) == "" {
		writeEvalError(w, 400, "name_required", "dataset name is required")
		return
	}
	record := &store.RAGEvalDatasetRecord{Name: strings.TrimSpace(request.Name), Description: request.Description, CreatedBy: evalIdentity(r)}
	if key != "" {
		record.ID = evalDeterministicID("rds_", record.CreatedBy, key)
	}
	if err := st.CreateDataset(r.Context(), record); err != nil {
		if key != "" {
			if existing, getErr := st.GetDataset(r.Context(), record.ID); getErr == nil {
				jsonResponse(w, 200, existing)
				return
			}
		}
		writeEvalError(w, 500, "create_failed", "could not create dataset")
		return
	}
	jsonResponse(w, 201, record)
}

func (s *Server) handleGetRAGEvalDataset(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	dataset, err := st.GetDataset(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	_, limit := evalListParams(r)
	versions, err := st.ListDatasetVersions(r.Context(), dataset.ID, r.URL.Query().Get("versionCursor"), limit)
	if err != nil {
		writeEvalError(w, 500, "list_failed", "could not list dataset versions")
		return
	}
	next := ""
	if len(versions) == limit {
		next = versions[len(versions)-1].ID
	}
	jsonResponse(w, 200, map[string]any{"dataset": dataset, "versions": maskEvalDatasetVersions(versions), "nextVersionCursor": next})
}

func (s *Server) handleDeleteRAGEvalDataset(w http.ResponseWriter, r *http.Request) {
	if s.ragEvalDatasets == nil {
		writeEvalError(w, http.StatusServiceUnavailable, "eval_unavailable", "RAG evaluation dataset service is unavailable")
		return
	}
	changed, err := s.ragEvalDatasets.Tombstone(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	if !changed {
		writeEvalError(w, http.StatusNotFound, "not_found", "evaluation dataset not found or already deleted")
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]any{"ok": true, "status": "tombstoned"})
}

type inlineEvalDocument struct {
	ExternalID    string `json:"externalId"`
	FileName      string `json:"fileName"`
	MediaType     string `json:"mediaType"`
	ContentBase64 string `json:"contentBase64"`
}

type evalDatasetVersionDTO struct {
	ID, DatasetID, Status, SourceType, Track, CorpusSHA256 string
	Version, CaseCount, DocumentCount, TotalBytes          int64
	SourceConfigJSON, SelectorFingerprint                  string
	CreatedBy                                              string
	CreatedAt                                              time.Time
}

func maskEvalDatasetVersions(items []store.RAGEvalDatasetVersionRecord) []evalDatasetVersionDTO {
	out := make([]evalDatasetVersionDTO, 0, len(items))
	for _, item := range items {
		out = append(out, evalDatasetVersionDTO{ID: item.ID, DatasetID: item.DatasetID, Status: item.Status, SourceType: item.SourceType, Track: item.Track, SourceConfigJSON: item.SourceConfigJSON, SelectorFingerprint: item.SelectorFingerprint, CorpusSHA256: item.CorpusSHA256, Version: item.Version, CaseCount: item.CaseCount, DocumentCount: item.DocumentCount, TotalBytes: item.TotalBytes, CreatedBy: item.CreatedBy, CreatedAt: item.CreatedAt})
	}
	return out
}

func (s *Server) handleCreateRAGEvalDatasetVersion(w http.ResponseWriter, r *http.Request) {
	if s.ragEvalDatasets == nil {
		writeEvalError(w, 503, "dataset_import_unavailable", "dataset import service is unavailable")
		return
	}
	var request struct {
		Version   int64                `json:"version"`
		Manifest  json.RawMessage      `json:"manifest"`
		Documents []inlineEvalDocument `json:"documents"`
	}
	if !decodeEvalJSON(w, r, 64<<20, &request) {
		return
	}
	uploads := make([]eval.DatasetDocumentUpload, 0, len(request.Documents))
	for _, document := range request.Documents {
		content, err := base64.StdEncoding.DecodeString(document.ContentBase64)
		if err != nil {
			writeEvalError(w, 400, "invalid_document", "document contentBase64 is invalid")
			return
		}
		uploads = append(uploads, eval.DatasetDocumentUpload{ExternalID: document.ExternalID, FileName: document.FileName, MediaType: document.MediaType, SizeBytes: int64(len(content)), Reader: bytes.NewReader(content)})
	}
	result, err := s.ragEvalDatasets.ImportCanonical(r.Context(), eval.DatasetImportRequest{DatasetID: r.PathValue("id"), Version: request.Version, CreatedBy: evalIdentity(r), Manifest: bytes.NewReader(request.Manifest), Documents: uploads})
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	jsonResponse(w, http.StatusCreated, result)
}

func (s *Server) handleValidateRAGEvalDatasetVersion(w http.ResponseWriter, r *http.Request) {
	s.handleRAGEvalDatasetValidation(w, r)
}
func (s *Server) handleRAGEvalDatasetValidation(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	version, err := st.GetDatasetVersion(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	if !json.Valid([]byte(version.ValidationReportJSON)) {
		writeEvalError(w, 500, "invalid_validation_report", "stored validation report is invalid")
		return
	}
	var report any
	_ = json.Unmarshal([]byte(version.ValidationReportJSON), &report)
	jsonResponse(w, 200, map[string]any{"versionId": version.ID, "status": version.Status, "report": report})
}

func (s *Server) handleListRAGEvalProfiles(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	cursor, limit := evalListParams(r)
	items, err := st.ListProfiles(r.Context(), cursor, limit)
	if err != nil {
		writeEvalError(w, 500, "list_failed", "could not list profiles")
		return
	}
	next := ""
	if len(items) == limit {
		next = items[len(items)-1].ID
	}
	jsonResponse(w, 200, map[string]any{"items": items, "nextCursor": next})
}

func (s *Server) handleCreateRAGEvalProfile(w http.ResponseWriter, r *http.Request) {
	if !s.ragCfg.Evaluation.Enabled {
		writeEvalError(w, 503, "eval_disabled", "RAG evaluation is disabled")
		return
	}
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	key, valid := evalIdempotencyKey(r)
	if !valid {
		writeEvalError(w, 400, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 characters")
		return
	}
	var request struct {
		Name    string          `json:"name"`
		Profile json.RawMessage `json:"profile"`
	}
	if !decodeEvalJSON(w, r, 1<<20, &request) {
		return
	}
	data, err := config.DecodeRAGEvalProfile(request.Profile)
	if err != nil {
		writeEvalError(w, 400, "invalid_profile", err.Error())
		return
	}
	fingerprint, err := eval.ProfileFingerprint(eval.Profile{Name: request.Name, Data: data})
	if err != nil {
		writeEvalError(w, 400, "invalid_profile", err.Error())
		return
	}
	canonical, _ := json.Marshal(data)
	record := &store.RAGEvalProfileRecord{Name: strings.TrimSpace(request.Name), ProfileJSON: string(canonical), Fingerprint: fingerprint, CreatedBy: evalIdentity(r)}
	if key != "" {
		record.ID = evalDeterministicID("rep_", record.CreatedBy, key)
	}
	if err = st.CreateProfile(r.Context(), record); err != nil {
		if key != "" {
			if existing, getErr := st.GetProfile(r.Context(), record.ID); getErr == nil {
				jsonResponse(w, 200, existing)
				return
			}
		}
		writeEvalError(w, 500, "create_failed", "could not create profile")
		return
	}
	jsonResponse(w, 201, record)
}

func (s *Server) handleListRAGEvalRuns(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	cursor, limit := evalListParams(r)
	status := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("status")))
	if status != "" {
		allowed := map[string]bool{store.RAGEvalRunQueued: true, store.RAGEvalRunRunning: true, store.RAGEvalRunSucceeded: true, store.RAGEvalRunFailed: true, store.RAGEvalRunCancelled: true, store.RAGEvalRunBudgetExceeded: true}
		if !allowed[status] {
			writeEvalError(w, 400, "invalid_filter", "unsupported run status filter")
			return
		}
	}
	items, next, err := listRAGEvalRunPage(r.Context(), cursor, limit, status, st.ListRuns)
	if err != nil {
		writeEvalError(w, 500, "list_failed", "could not list runs")
		return
	}
	jsonResponse(w, 200, map[string]any{"items": items, "nextCursor": next})
}

func listRAGEvalRunPage(ctx context.Context, cursor string, limit int, status string, list func(context.Context, string, int) ([]store.RAGEvalRunRecord, error)) ([]store.RAGEvalRunRecord, string, error) {
	items := make([]store.RAGEvalRunRecord, 0, limit)
	scanCursor := cursor
	batchLimit := limit
	if status != "" {
		batchLimit = 200
	}
	for len(items) < limit {
		batch, err := list(ctx, scanCursor, batchLimit)
		if err != nil {
			return nil, "", err
		}
		if len(batch) == 0 {
			return items, "", nil
		}
		for _, item := range batch {
			scanCursor = item.ID
			if status == "" || item.Status == status {
				items = append(items, item)
				if len(items) == limit {
					return items, scanCursor, nil
				}
			}
		}
		if len(batch) < batchLimit {
			return items, "", nil
		}
	}
	return items, scanCursor, nil
}

func (s *Server) handleCreateRAGEvalRun(w http.ResponseWriter, r *http.Request) {
	if !s.ragCfg.Evaluation.Enabled {
		writeEvalError(w, 503, "eval_disabled", "RAG evaluation is disabled")
		return
	}
	if s.ragEvalRunner == nil {
		writeEvalError(w, 503, "runner_unavailable", "RAG evaluation runner is unavailable")
		return
	}
	key, valid := evalIdempotencyKey(r)
	if !valid {
		writeEvalError(w, 400, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 characters")
		return
	}
	var request struct {
		DatasetVersionID  string   `json:"datasetVersionId"`
		BaselineRunID     string   `json:"baselineRunId"`
		Mode              string   `json:"mode"`
		ProfileID         string   `json:"profileId"`
		IndexGenerationID string   `json:"indexGenerationId"`
		Metrics           []string `json:"metrics"`
	}
	if !decodeEvalJSON(w, r, 256<<10, &request) {
		return
	}
	actor := evalIdentity(r)
	id := ""
	if key != "" {
		id = evalDeterministicID("rer_", actor, key)
	}
	record, err := s.ragEvalRunner.CreateRun(r.Context(), eval.CreateRunRequest{ID: id, DatasetVersionID: request.DatasetVersionID, BaselineRunID: request.BaselineRunID, Mode: eval.RunMode(request.Mode), ProfileID: request.ProfileID, IndexGenerationID: request.IndexGenerationID, Metrics: request.Metrics, CreatedBy: actor})
	if err != nil {
		if key != "" {
			if st, ok := s.evalService(w); ok {
				if existing, getErr := st.GetRun(r.Context(), id); getErr == nil {
					jsonResponse(w, 200, existing)
					return
				}
			}
		}
		writeEvalServiceError(w, err)
		return
	}
	jsonResponse(w, 201, record)
}

func (s *Server) handleGetRAGEvalRun(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	record, err := st.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	var metrics []string
	_ = json.Unmarshal([]byte(record.RequestedMetricsJSON), &metrics)
	aggregates := map[string]eval.Aggregate{}
	for _, metric := range metrics {
		if aggregate, aggregateErr := st.Aggregate(r.Context(), record.ID, metric); aggregateErr == nil {
			aggregates[metric] = aggregate
		}
	}
	jsonResponse(w, 200, map[string]any{"run": record, "aggregates": aggregates})
}

func (s *Server) handleDeleteRAGEvalRun(w http.ResponseWriter, r *http.Request) {
	service, ok := s.evalService(w)
	if !ok {
		return
	}
	changed, err := service.DeleteRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	if !changed {
		writeEvalError(w, http.StatusConflict, "run_not_terminal", "only a terminal evaluation run can be deleted")
		return
	}
	jsonResponse(w, http.StatusAccepted, map[string]any{"ok": true, "status": "tombstoned"})
}

func (s *Server) handleCancelRAGEvalRun(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	if _, valid := evalIdempotencyKey(r); !valid {
		writeEvalError(w, 400, "invalid_idempotency_key", "Idempotency-Key must contain 8 to 128 characters")
		return
	}
	changed, err := st.CancelRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeEvalError(w, 500, "cancel_failed", "could not cancel run")
		return
	}
	if !changed {
		run, getErr := st.GetRun(r.Context(), r.PathValue("id"))
		if getErr != nil {
			writeEvalServiceError(w, getErr)
			return
		}
		if !run.CancelRequestedAt.Valid && (run.Status == store.RAGEvalRunQueued || run.Status == store.RAGEvalRunRunning) {
			writeEvalError(w, 409, "run_not_cancellable", "run is not cancellable")
			return
		}
	}
	jsonResponse(w, 202, map[string]any{"ok": true})
}

type evalCaseDTO struct {
	CaseID       string          `json:"caseId"`
	Response     string          `json:"response,omitempty"`
	Contexts     json.RawMessage `json:"contexts"`
	Citations    json.RawMessage `json:"citations"`
	Status       string          `json:"status"`
	ErrorCode    string          `json:"errorCode,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	LatencyMS    int64           `json:"latencyMs"`
	Usage        json.RawMessage `json:"usage"`
	SearchTrace  json.RawMessage `json:"searchTrace,omitempty"`
	AnswerTrace  json.RawMessage `json:"answerTrace,omitempty"`
	Metrics      []evalMetricDTO `json:"metrics"`
}

type evalMetricDTO struct {
	Name, Version, Status, Reason string
	Value                         *float64
	Details                       json.RawMessage
}

func rawEvalJSON(value, fallback string) json.RawMessage {
	if json.Valid([]byte(value)) {
		return json.RawMessage(value)
	}
	return json.RawMessage(fallback)
}
func (s *Server) handleListRAGEvalRunCases(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	cursor, limit := evalListParams(r)
	status := r.URL.Query().Get("status")
	if status != "" && status != store.RAGEvalCaseOK && status != store.RAGEvalCaseError {
		writeEvalError(w, 400, "invalid_filter", "unsupported case status filter")
		return
	}
	runID := r.PathValue("id")
	items, next, err := listRAGEvalCasePage(r.Context(), runID, cursor, limit, status, st.ListCaseResults)
	if err != nil {
		writeEvalError(w, 500, "list_failed", "could not list run cases")
		return
	}
	includeTraces := r.URL.Query().Get("includeTraces") == "true"
	metricItems, metricErr := listRAGEvalMetricsForCases(r.Context(), runID, items, st.ListMetricResults)
	if metricErr != nil {
		writeEvalError(w, 500, "list_failed", "could not list case metrics")
		return
	}
	metricsByCase := map[string][]evalMetricDTO{}
	for _, metric := range metricItems {
		item := evalMetricDTO{Name: metric.MetricName, Version: metric.MetricVersion, Status: metric.Status, Reason: metric.Reason, Details: rawEvalJSON(metric.DetailsJSON, "{}")}
		if metric.Value.Valid {
			value := metric.Value.Float64
			item.Value = &value
		}
		metricsByCase[metric.CaseID] = append(metricsByCase[metric.CaseID], item)
	}
	out := make([]evalCaseDTO, 0, len(items))
	for _, item := range items {
		metrics := metricsByCase[item.CaseID]
		if metrics == nil {
			metrics = []evalMetricDTO{}
		}
		dto := evalCaseDTO{CaseID: item.CaseID, Response: item.Response, Contexts: rawEvalJSON(item.ContextsJSON, "[]"), Citations: rawEvalJSON(item.CitationsJSON, "[]"), Status: item.Status, ErrorCode: item.ErrorCode, ErrorMessage: item.ErrorMessage, LatencyMS: item.LatencyMS, Usage: rawEvalJSON(item.UsageJSON, "{}"), Metrics: metrics}
		if includeTraces {
			dto.SearchTrace = rawEvalJSON(item.SearchTraceJSON, "{}")
			dto.AnswerTrace = rawEvalJSON(item.AnswerTraceJSON, "{}")
		}
		out = append(out, dto)
	}
	jsonResponse(w, 200, map[string]any{"items": out, "nextCursor": next})
}

func listRAGEvalCasePage(ctx context.Context, runID, cursor string, limit int, status string, list func(context.Context, string, string, int) ([]store.RAGEvalCaseResultRecord, error)) ([]store.RAGEvalCaseResultRecord, string, error) {
	items := make([]store.RAGEvalCaseResultRecord, 0, limit)
	scanCursor := cursor
	batchLimit := limit
	if status != "" {
		batchLimit = 200
	}
	for len(items) < limit {
		batch, err := list(ctx, runID, scanCursor, batchLimit)
		if err != nil {
			return nil, "", err
		}
		if len(batch) == 0 {
			return items, "", nil
		}
		for _, item := range batch {
			scanCursor = item.CaseID
			if status == "" || item.Status == status {
				items = append(items, item)
				if len(items) == limit {
					return items, scanCursor, nil
				}
			}
		}
		if len(batch) < batchLimit {
			return items, "", nil
		}
	}
	return items, scanCursor, nil
}

func listRAGEvalMetricsForCases(ctx context.Context, runID string, cases []store.RAGEvalCaseResultRecord, list func(context.Context, string, string, int) ([]store.RAGEvalMetricResultRecord, error)) ([]store.RAGEvalMetricResultRecord, error) {
	if len(cases) == 0 {
		return nil, nil
	}
	wanted := make(map[string]struct{}, len(cases))
	for _, item := range cases {
		wanted[item.CaseID] = struct{}{}
	}
	lastCaseID := cases[len(cases)-1].CaseID
	cursor := cases[0].CaseID + "::"
	out := make([]store.RAGEvalMetricResultRecord, 0, len(cases))
	for {
		batch, err := list(ctx, runID, cursor, 200)
		if err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			return out, nil
		}
		for _, item := range batch {
			if item.CaseID > lastCaseID {
				return out, nil
			}
			if _, ok := wanted[item.CaseID]; ok {
				out = append(out, item)
			}
		}
		if len(batch) < 200 {
			return out, nil
		}
		last := batch[len(batch)-1]
		cursor = store.RAGEvalMetricCursor(last)
	}
}

func (s *Server) handleCompareRAGEvalRuns(w http.ResponseWriter, r *http.Request) {
	metric := strings.TrimSpace(r.URL.Query().Get("metric"))
	if metric == "" || len(metric) > 64 {
		writeEvalError(w, 400, "metric_required", "bounded metric query parameter is required")
		return
	}
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	result, err := st.Compare(r.Context(), r.PathValue("baselineId"), r.PathValue("id"), metric)
	if err != nil {
		writeEvalServiceError(w, err)
		return
	}
	jsonResponse(w, 200, result)
}

type evalExportAuthorizer struct{}

func (evalExportAuthorizer) AuthorizeRAGEvalExport(_ context.Context, actor string, _ *store.RAGEvalRunRecord) error {
	if strings.TrimSpace(actor) == "" {
		return errors.New("reauthorization required")
	}
	return nil
}
func (s *Server) handleExportRAGEvalRun(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	format := eval.ExportFormat(r.URL.Query().Get("format"))
	if format == "" {
		format = eval.ExportJSON
	}
	if format != eval.ExportJSON && format != eval.ExportCSV {
		writeEvalError(w, 400, "invalid_format", "format must be json or csv")
		return
	}
	if format == eval.ExportCSV {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	} else {
		w.Header().Set("Content-Type", "application/json")
	}
	w.Header().Set("Content-Disposition", "attachment; filename=rag-eval-"+r.PathValue("id")+"."+string(format))
	_ = st.Export(r.Context(), evalIdentity(r), r.PathValue("id"), format, r.URL.Query().Get("includeTraces") == "true", w, evalExportAuthorizer{})
}

func (s *Server) handleListRAGPolicies(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalService(w)
	if !ok {
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	if limit < 1 {
		limit = 1
	}
	if limit > 100 {
		limit = 100
	}
	response := map[string]any{}
	for _, kind := range []string{store.RAGPolicyRuntime, store.RAGPolicyIngestion} {
		active, err := st.ActivePolicy(r.Context(), kind)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			writeEvalError(w, 500, "list_failed", "could not list policies")
			return
		}
		audits, auditErr := st.PolicyAudits(r.Context(), kind, limit)
		if auditErr != nil {
			writeEvalError(w, 500, "list_failed", "could not list policy audit")
			return
		}
		response[kind] = map[string]any{"active": active, "audit": audits}
	}
	jsonResponse(w, 200, response)
}
