package setup

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/qs3c/bkcrab/internal/rag"
	"github.com/qs3c/bkcrab/internal/store"
)

func (s *Server) registerRAGPolicyRoutes(mux *http.ServeMux, gate func(http.HandlerFunc) http.HandlerFunc) {
	mux.HandleFunc("POST /api/admin/rag-policies/runtime/promotions", gate(s.handlePromoteRAGRuntimePolicy))
	mux.HandleFunc("POST /api/admin/rag-policies/runtime/rollback", gate(s.handleRollbackRAGRuntimePolicy))
	mux.HandleFunc("POST /api/admin/rag-policies/ingestion/promotions", gate(s.handlePromoteRAGIngestionPolicy))
}

func (s *Server) handlePromoteRAGIngestionPolicy(w http.ResponseWriter, r *http.Request) {
	if s.ragPolicyPromotion == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "RAG policy promotion is unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var request struct {
		RunID             string `json:"runId"`
		ProfileID         string `json:"profileId"`
		ConfirmationRunID string `json:"confirmationRunId"`
		Note              string `json:"note"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid promotion request"})
		return
	}
	result, err := s.ragPolicyPromotion.PromoteIngestion(r.Context(), rag.IngestionPromotionRequest{RunID: request.RunID, ProfileID: request.ProfileID, ConfirmationRunID: request.ConfirmationRunID, ActorID: evalIdentity(r), Note: request.Note})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, rag.ErrPolicyActivationConflict) {
			status = http.StatusConflict
		}
		if errors.Is(err, rag.ErrPromotionGatesUnconfigured) {
			status = http.StatusServiceUnavailable
		}
		jsonResponse(w, status, map[string]any{"ok": false, "error": err.Error(), "gateReport": result.GateReport})
		return
	}
	jsonResponse(w, http.StatusCreated, result)
}

func (s *Server) handlePromoteRAGRuntimePolicy(w http.ResponseWriter, r *http.Request) {
	if s.ragPolicyPromotion == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "RAG policy promotion is unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var request struct {
		RunID             string   `json:"runId"`
		ProfileID         string   `json:"profileId"`
		ConfirmationRunID string   `json:"confirmationRunId"`
		Fields            []string `json:"fields"`
		Note              string   `json:"note"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid promotion request"})
		return
	}
	result, err := s.ragPolicyPromotion.PromoteRuntime(r.Context(), rag.RuntimePromotionRequest{RunID: request.RunID, ProfileID: request.ProfileID, ConfirmationRunID: request.ConfirmationRunID, Fields: request.Fields, ActorID: evalIdentity(r), Note: request.Note})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, rag.ErrPolicyActivationConflict) {
			status = http.StatusConflict
		}
		if errors.Is(err, rag.ErrPromotionGatesUnconfigured) {
			status = http.StatusServiceUnavailable
		}
		jsonResponse(w, status, map[string]any{"ok": false, "error": err.Error(), "gateReport": result.GateReport})
		return
	}
	jsonResponse(w, http.StatusCreated, result)
}

func (s *Server) handleRollbackRAGRuntimePolicy(w http.ResponseWriter, r *http.Request) {
	if s.ragPolicyPromotion == nil {
		jsonResponse(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "RAG policy promotion is unavailable"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	var request struct {
		ExpectedVersion int64  `json:"expectedVersion"`
		TargetVersion   int64  `json:"targetVersion"`
		Note            string `json:"note"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid rollback request"})
		return
	}
	if err := s.ragPolicyPromotion.RollbackRuntime(r.Context(), request.ExpectedVersion, request.TargetVersion, evalIdentity(r), request.Note); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, rag.ErrPolicyActivationConflict) {
			status = http.StatusConflict
		}
		jsonResponse(w, status, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStartRAGKBPolicySync(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) || !s.requireWritable(w, r) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var request struct {
		TargetPolicyVersion int64 `json:"targetPolicyVersion"`
		Confirm             bool  `json:"confirm"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || !request.Confirm {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "explicit policy sync confirmation is required"})
		return
	}
	task, err := s.rag.StartKBPolicySync(r.Context(), ragOwnerID(identity), r.PathValue("id"), request.TargetPolicyVersion)
	if err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusAccepted, ragPolicySyncResponse(task))
}

func (s *Server) handleGetRAGKBPolicySync(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	task, err := s.rag.GetKBPolicySyncTask(r.Context(), ragOwnerID(identity), r.PathValue("id"), r.PathValue("taskId"))
	if err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, ragPolicySyncResponse(task))
}

func ragPolicySyncResponse(task *store.RAGPolicySyncTaskRecord) map[string]any {
	if task == nil {
		return map[string]any{}
	}
	var progress, estimate any = map[string]any{}, map[string]any{}
	if json.Valid([]byte(task.ProgressJSON)) {
		_ = json.Unmarshal([]byte(task.ProgressJSON), &progress)
	}
	if json.Valid([]byte(task.EstimateJSON)) {
		_ = json.Unmarshal([]byte(task.EstimateJSON), &estimate)
	}
	return map[string]any{"id": task.ID, "kbId": task.KBID, "sourceGenerationId": task.SourceGenerationID, "targetGenerationId": task.TargetGenerationID, "targetPolicyVersion": task.TargetPolicyVersion, "status": task.Status, "progress": progress, "estimate": estimate, "cancelRequested": task.CancelRequestedAt.Valid, "errorCode": task.ErrorCode, "errorMessage": func() string {
		if task.Status == store.RAGPolicySyncFailed {
			return "策略同步失败；旧索引仍正常"
		}
		return ""
	}(), "createdAt": task.CreatedAt, "startedAt": task.StartedAt, "finishedAt": task.FinishedAt}
}

func (s *Server) handleCancelRAGKBPolicySync(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) || !s.requireWritable(w, r) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	if err := s.rag.CancelKBPolicySync(r.Context(), ragOwnerID(identity), r.PathValue("id"), r.PathValue("taskId")); err != nil {
		writeRAGError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRollbackRAGKBPolicy(w http.ResponseWriter, r *http.Request) {
	if !s.requireRAG(w) || !s.requireWritable(w, r) {
		return
	}
	identity, ok := ragIdentity(r)
	if !ok {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "unauthorized"})
		return
	}
	var request struct {
		TargetGenerationID   string `json:"targetGenerationId"`
		ExpectedGenerationID string `json:"expectedGenerationId"`
		Note                 string `json:"note"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid rollback request"})
		return
	}
	if err := s.rag.RollbackKBPolicy(r.Context(), ragOwnerID(identity), r.PathValue("id"), request.TargetGenerationID, request.ExpectedGenerationID, request.Note); err != nil {
		writeRAGError(w, err)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
