package setup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/qs3c/bkclaw/internal/store"
)

// Projects are per-(user, agent) named workspace folders that group
// chat sessions. Every chat in the same project shares one
// workspace dir at workspaces/<agent>/projects/<pid>/, so notes /
// generated files persist across the project's chats — that's the
// whole point of the feature.
//
// Endpoints exposed below:
//
//	GET    /api/agents/{id}/projects                   — list (caller's own)
//	POST   /api/agents/{id}/projects                   — create
//	PATCH  /api/agents/{id}/projects/{pid}             — rename / re-describe
//	DELETE /api/agents/{id}/projects/{pid}             — delete (blocked when chats remain)
//
// Project chats are minted lazily: clicking "New chat in project" in
// the sidebar just navigates to `/agents/<id>/chat/?project=<pid>`,
// and the very first user message carries `projectId` in the chat
// request body. The first SaveSession that fires from there stamps
// the new sessions row with project_id; subsequent saves leave it
// untouched (ON CONFLICT in the SQL upsert preserves it). No
// pre-create endpoint — keeps "user opened New chat and walked
// away" from littering the sidebar with empty rows.

// generateProjectID mints an opaque project_id matching the
// `<prefix>_<hex20>` shape that users (u_) and agents (agt_) use, so
// IDs are visually consistent across the platform. ~80 bits of entropy
// — collision-resistant at platform scale; we don't probe the store
// for uniqueness because the caller already passed requireAgentOwner.
func generateProjectID() string {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand should never fail on supported platforms; if it
		// does, surface a recognizable sentinel rather than silently
		// minting "proj_" (which would collide with itself across
		// concurrent calls).
		return "proj_rngerror"
	}
	return "proj_" + hex.EncodeToString(buf[:])
}

// trimProjectName strips wrapping whitespace and caps the length so a
// rogue 64KB body doesn't end up in the sidebar.
func trimProjectName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func trimProjectDescription(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 4000 {
		s = s[:4000]
	}
	return s
}

func projectToJSON(p *store.ProjectRecord) map[string]any {
	return map[string]any{
		"id":          p.ID,
		"name":        p.Name,
		"description": p.Description,
		"createdAt":   p.CreatedAt,
		"updatedAt":   p.UpdatedAt,
	}
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	uid := s.effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	rows, err := s.dataStore.ListProjects(r.Context(), uid, id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		out = append(out, projectToJSON(&rows[i]))
	}
	jsonResponse(w, http.StatusOK, map[string]any{"projects": out})
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	// Readable-not-owner is enough: projects are keyed on (user_id,
	// agent_id, project_id), so a viewer creating a project on a shared
	// agent only adds rows under THEIR user_id and can never touch the
	// owner's project list. Same reasoning for update / delete below.
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	uid := s.effectiveUserID(r)
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	name := trimProjectName(req.Name)
	if name == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name is required"})
		return
	}
	rec := &store.ProjectRecord{
		UserID:      uid,
		AgentID:     id,
		ID:          generateProjectID(),
		Name:        name,
		Description: trimProjectDescription(req.Description),
	}
	if err := s.dataStore.SaveProject(r.Context(), rec); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	// Re-read to get authoritative created_at / updated_at the DB
	// stamped on the row, instead of returning zero times.
	saved, err := s.dataStore.GetProject(r.Context(), uid, id, rec.ID)
	if err != nil || saved == nil {
		// Fall back to the in-memory copy — IDs and name are correct,
		// just timestamps are zero. Still better than 500'ing the
		// caller after a successful insert.
		jsonResponse(w, http.StatusOK, projectToJSON(rec))
		return
	}
	jsonResponse(w, http.StatusOK, projectToJSON(saved))
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	pid := r.PathValue("pid")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	uid := s.effectiveUserID(r)
	existing, err := s.dataStore.GetProject(r.Context(), uid, id, pid)
	if err != nil || existing == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "project not found"})
		return
	}
	// PATCH semantics: only the fields the caller sent get updated.
	// Use pointers so we can distinguish "not sent" from "sent empty
	// string" — the latter is a legitimate clear of the description.
	var req struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name != nil {
		n := trimProjectName(*req.Name)
		if n == "" {
			jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "name cannot be empty"})
			return
		}
		existing.Name = n
	}
	if req.Description != nil {
		existing.Description = trimProjectDescription(*req.Description)
	}
	if err := s.dataStore.SaveProject(r.Context(), existing); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	saved, _ := s.dataStore.GetProject(r.Context(), uid, id, pid)
	if saved == nil {
		saved = existing
	}
	jsonResponse(w, http.StatusOK, projectToJSON(saved))
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	if !s.requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	pid := r.PathValue("pid")
	if !s.requireAgentReadable(w, r, id) {
		return
	}
	uid := s.effectiveUserID(r)
	// Refuse to delete a project that still owns chats. Cascade /
	// soft-detach are deliberately not exposed — v1 keeps the
	// destructive action behind an explicit "delete chats first" step
	// so a slip on the trash icon can't nuke a survey's worth of
	// notes.
	n, err := s.dataStore.CountProjectSessions(r.Context(), uid, id, pid)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if n > 0 {
		jsonResponse(w, http.StatusConflict, map[string]any{
			"error":        "project still has chats",
			"sessionCount": n,
		})
		return
	}
	if err := s.dataStore.DeleteProject(r.Context(), uid, id, pid); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
