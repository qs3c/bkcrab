package setup

import (
	"encoding/json"
	"net/http"

	"github.com/qs3c/bkcrab/internal/agent"
)

// Stopping is explicit; losing a POST/SSE connection does not stop its worker.
func (s *Server) handleChatStop(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"stopped": ag.StopWebTurn(req.SessionID)})
}

func (s *Server) handleChatStatus(w http.ResponseWriter, r *http.Request) {
	ag := s.resolveAgent(r, r.URL.Query().Get("agentId"))
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	state := ag.WebTurnState(r.URL.Query().Get("sessionId"))
	jsonResponse(w, http.StatusOK, map[string]any{"active": state != agent.TurnIdle, "state": state})
}
