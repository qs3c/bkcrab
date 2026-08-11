package setup

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/qs3c/bkcrab/internal/rag"
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
