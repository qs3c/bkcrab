package setup

import (
	"net/http"
	"strconv"

	"github.com/fastclaw-ai/fastclaw/internal/usage"
)

// rangeFromQuery parses ?range=24h|7d|30d (default 7d) into a usage.Range
// of last-N days. We don't expose precise hour windows on the admin
// dashboard — daily buckets is good enough for "who burned what".
func rangeFromQuery(r *http.Request) usage.Range {
	switch r.URL.Query().Get("range") {
	case "24h":
		return usage.LastN(1)
	case "30d":
		return usage.LastN(30)
	default:
		return usage.LastN(7)
	}
}

// limitFromQuery clamps ?limit= to [1, 100], defaulting to 10.
func limitFromQuery(r *http.Request) int {
	v, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || v <= 0 {
		return 10
	}
	if v > 100 {
		return 100
	}
	return v
}

// handleGetUsage returns the headline numbers for the admin dashboard:
// total tokens, plus top agents and top users for the requested time
// window. Wrapped by requireSuperAdmin in server.go.
func (s *Server) handleGetUsage(w http.ResponseWriter, r *http.Request) {
	if s.usage == nil {
		jsonResponse(w, http.StatusOK, map[string]any{
			"totals":    usage.Totals{},
			"topAgents": []usage.Rank{},
			"topUsers":  []usage.Rank{},
		})
		return
	}
	rng := rangeFromQuery(r)
	limit := limitFromQuery(r)
	totals, err := s.usage.Totals(r.Context(), rng)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	topAgents, err := s.usage.TopAgents(r.Context(), rng, limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	topUsers, err := s.usage.TopUsers(r.Context(), rng, limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"range":     r.URL.Query().Get("range"),
		"totals":    totals,
		"topAgents": topAgents,
		"topUsers":  topUsers,
	})
}

// handleGetAgentUsage returns per-session token rollups for one agent
// — the data behind the "Token Usage" tab in the agent settings
// dialog. Owner-gated via requireAgentOwner, so chat viewers of a
// public agent don't get to see the owner's other sessions. The
// `sessions` list is a Rank[] keyed by session_key (rendered with
// session title client-side after a name lookup).
func (s *Server) handleGetAgentUsage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.requireAgentOwner(w, r, id)
	if rec == nil {
		return
	}
	if s.usage == nil {
		jsonResponse(w, http.StatusOK, map[string]any{
			"range":    r.URL.Query().Get("range"),
			"totals":   nil,
			"sessions": []any{},
		})
		return
	}
	rng := rangeFromQuery(r)
	limit := limitFromQuery(r)
	if limit < 50 {
		// Sessions list is the headline view on this tab; default
		// limit (10 from limitFromQuery) is too short. Cap at 50
		// rows unless the caller explicitly asked for fewer.
		limit = 50
	}
	sessions, err := s.usage.SessionsForAgent(r.Context(), id, "", rng, limit)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"range":    r.URL.Query().Get("range"),
		"agentId":  id,
		"sessions": sessions,
	})
}
