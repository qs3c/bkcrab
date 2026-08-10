package setup

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/eval"
	"github.com/qs3c/bkcrab/internal/store"
)

type ragEvaluationStore interface {
	CreateRAGEvalDataset(context.Context, *store.RAGEvalDatasetRecord) error
	ListRAGEvalDatasets(context.Context, string, int) ([]store.RAGEvalDatasetRecord, error)
	CreateRAGEvalProfile(context.Context, *store.RAGEvalProfileRecord) error
	ListRAGEvalProfiles(context.Context, string, int) ([]store.RAGEvalProfileRecord, error)
	CreateRAGEvalRun(context.Context, *store.RAGEvalRunRecord) error
	ListRAGEvalRuns(context.Context, string, int) ([]store.RAGEvalRunRecord, error)
	GetRAGEvalRun(context.Context, string) (*store.RAGEvalRunRecord, error)
	RequestCancelRAGEvalRun(context.Context, string) (bool, error)
}

func (s *Server) registerRAGEvaluationRoutes(mux *http.ServeMux, gate func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("GET /api/admin/rag-evals/capabilities", gate(s.handleRAGEvalCapabilities))
	mux.HandleFunc("GET /api/admin/rag-evals/datasets", gate(s.handleListRAGEvalDatasets))
	mux.HandleFunc("POST /api/admin/rag-evals/datasets", gate(s.handleCreateRAGEvalDataset))
	mux.HandleFunc("GET /api/admin/rag-evals/profiles", gate(s.handleListRAGEvalProfiles))
	mux.HandleFunc("POST /api/admin/rag-evals/profiles", gate(s.handleCreateRAGEvalProfile))
	mux.HandleFunc("GET /api/admin/rag-evals/runs", gate(s.handleListRAGEvalRuns))
	mux.HandleFunc("POST /api/admin/rag-evals/runs", gate(s.handleCreateRAGEvalRun))
	mux.HandleFunc("GET /api/admin/rag-evals/runs/{id}", gate(s.handleGetRAGEvalRun))
	mux.HandleFunc("POST /api/admin/rag-evals/runs/{id}/cancel", gate(s.handleCancelRAGEvalRun))
}

func (s *Server) evalStore(w http.ResponseWriter) (ragEvaluationStore, bool) {
	value, ok := s.dataStore.(ragEvaluationStore)
	if !ok {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "RAG evaluation store is unavailable"})
	}
	return value, ok
}
func evalListParams(r *http.Request) (string, int) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	return r.URL.Query().Get("cursor"), limit
}
func evalIdentity(r *http.Request) string {
	identity, _ := auth.FromContext(r.Context())
	return identity.UserID
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
func (s *Server) handleListRAGEvalDatasets(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalStore(w)
	if !ok {
		return
	}
	cursor, limit := evalListParams(r)
	items, err := st.ListRAGEvalDatasets(r.Context(), cursor, limit)
	if err != nil {
		jsonResponse(w, 500, map[string]any{"ok": false, "error": "list datasets failed"})
		return
	}
	jsonResponse(w, 200, map[string]any{"items": items, "nextCursor": nextDatasetCursor(items)})
}
func nextDatasetCursor(items []store.RAGEvalDatasetRecord) string {
	if len(items) == 0 {
		return ""
	}
	return items[len(items)-1].ID
}
func (s *Server) handleCreateRAGEvalDataset(w http.ResponseWriter, r *http.Request) {
	if !s.ragCfg.Evaluation.Enabled {
		jsonResponse(w, 503, map[string]any{"ok": false, "error": "RAG evaluation is disabled"})
		return
	}
	st, ok := s.evalStore(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var request struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || strings.TrimSpace(request.Name) == "" {
		jsonResponse(w, 400, map[string]any{"ok": false, "error": "invalid dataset"})
		return
	}
	record := &store.RAGEvalDatasetRecord{Name: strings.TrimSpace(request.Name), Description: request.Description, CreatedBy: evalIdentity(r)}
	if err := st.CreateRAGEvalDataset(r.Context(), record); err != nil {
		jsonResponse(w, 500, map[string]any{"ok": false, "error": "create dataset failed"})
		return
	}
	jsonResponse(w, 201, record)
}

func (s *Server) handleListRAGEvalProfiles(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalStore(w)
	if !ok {
		return
	}
	cursor, limit := evalListParams(r)
	items, err := st.ListRAGEvalProfiles(r.Context(), cursor, limit)
	if err != nil {
		jsonResponse(w, 500, map[string]any{"ok": false, "error": "list profiles failed"})
		return
	}
	next := ""
	if len(items) > 0 {
		next = items[len(items)-1].ID
	}
	jsonResponse(w, 200, map[string]any{"items": items, "nextCursor": next})
}
func (s *Server) handleCreateRAGEvalProfile(w http.ResponseWriter, r *http.Request) {
	if !s.ragCfg.Evaluation.Enabled {
		jsonResponse(w, 503, map[string]any{"ok": false, "error": "RAG evaluation is disabled"})
		return
	}
	st, ok := s.evalStore(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var request struct {
		Name    string          `json:"name"`
		Profile json.RawMessage `json:"profile"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		jsonResponse(w, 400, map[string]any{"ok": false, "error": "invalid profile"})
		return
	}
	data, err := config.DecodeRAGEvalProfile(request.Profile)
	if err != nil {
		jsonResponse(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	fingerprint, err := eval.ProfileFingerprint(eval.Profile{Name: request.Name, Data: data})
	if err != nil {
		jsonResponse(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	canonical, _ := json.Marshal(data)
	record := &store.RAGEvalProfileRecord{Name: strings.TrimSpace(request.Name), ProfileJSON: string(canonical), Fingerprint: fingerprint, CreatedBy: evalIdentity(r)}
	if err = st.CreateRAGEvalProfile(r.Context(), record); err != nil {
		jsonResponse(w, 500, map[string]any{"ok": false, "error": "create profile failed"})
		return
	}
	jsonResponse(w, 201, record)
}

func (s *Server) handleListRAGEvalRuns(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalStore(w)
	if !ok {
		return
	}
	cursor, limit := evalListParams(r)
	items, err := st.ListRAGEvalRuns(r.Context(), cursor, limit)
	if err != nil {
		jsonResponse(w, 500, map[string]any{"ok": false, "error": "list runs failed"})
		return
	}
	next := ""
	if len(items) > 0 {
		next = items[len(items)-1].ID
	}
	jsonResponse(w, 200, map[string]any{"items": items, "nextCursor": next})
}
func (s *Server) handleCreateRAGEvalRun(w http.ResponseWriter, r *http.Request) {
	if !s.ragCfg.Evaluation.Enabled {
		jsonResponse(w, 503, map[string]any{"ok": false, "error": "RAG evaluation is disabled"})
		return
	}
	st, ok := s.evalStore(w)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	var request struct {
		DatasetVersionID  string   `json:"datasetVersionId"`
		BaselineRunID     string   `json:"baselineRunId"`
		Mode              string   `json:"mode"`
		ProfileID         string   `json:"profileId"`
		IndexGenerationID string   `json:"indexGenerationId"`
		Metrics           []string `json:"metrics"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		jsonResponse(w, 400, map[string]any{"ok": false, "error": "invalid run"})
		return
	}
	metrics, _ := json.Marshal(request.Metrics)
	record := &store.RAGEvalRunRecord{DatasetVersionID: request.DatasetVersionID, BaselineRunID: request.BaselineRunID, Mode: request.Mode, ProfileID: request.ProfileID, IndexGenerationID: request.IndexGenerationID, RequestedMetricsJSON: string(metrics), ExecutionSnapshotJSON: "{}", CreatedBy: evalIdentity(r)}
	if err := st.CreateRAGEvalRun(r.Context(), record); err != nil {
		jsonResponse(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, 201, record)
}
func (s *Server) handleGetRAGEvalRun(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalStore(w)
	if !ok {
		return
	}
	record, err := st.GetRAGEvalRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, 200, record)
}
func (s *Server) handleCancelRAGEvalRun(w http.ResponseWriter, r *http.Request) {
	st, ok := s.evalStore(w)
	if !ok {
		return
	}
	changed, err := st.RequestCancelRAGEvalRun(r.Context(), r.PathValue("id"))
	if err != nil {
		jsonResponse(w, 500, map[string]any{"ok": false, "error": "cancel failed"})
		return
	}
	if !changed {
		jsonResponse(w, 409, map[string]any{"ok": false, "error": "run is not cancellable"})
		return
	}
	jsonResponse(w, 202, map[string]any{"ok": true})
}
