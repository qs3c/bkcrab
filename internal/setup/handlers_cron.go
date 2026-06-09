package setup

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/fastclaw-ai/fastclaw/internal/config"
	"github.com/fastclaw-ai/fastclaw/internal/store"
)

// --- Per-agent cron jobs (DB-backed) ---
//
// The legacy /api/cron set below reads jobs out of the user's flat
// fastclaw.json (cfg.CronJobs) — that's the install-time, statically-
// configured catalog. Agents that schedule work at runtime via the
// create_cron_job tool persist into the cron_jobs DB table instead, and
// the cron.Scheduler (which actually fires them) only watches the DB.
// So those agent-authored jobs were invisible to the dashboard.
// handleListAgentCronJobs surfaces them at /api/agents/{id}/cron.

func (s *Server) handleListAgentCronJobs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	jobs, err := s.dataStore.ListCronJobsByAgent(r.Context(), id)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if jobs == nil {
		jobs = []store.CronJobRecord{}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"jobs": jobs})
}

func (s *Server) handleDeleteAgentCronJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	jobID := r.PathValue("jobId")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	// Verify the job belongs to this agent before deleting — otherwise
	// the path param could be used to delete jobs the caller doesn't
	// own (the cron table has no user_id; we gate via agent ownership).
	job, err := s.dataStore.GetCronJob(r.Context(), jobID)
	if err != nil || job == nil || job.AgentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "job not found for this agent"})
		return
	}
	if err := s.dataStore.DeleteCronJob(r.Context(), jobID); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleToggleAgentCronJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	jobID := r.PathValue("jobId")
	if s.requireAgentOwner(w, r, id) == nil {
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "invalid request"})
		return
	}
	job, err := s.dataStore.GetCronJob(r.Context(), jobID)
	if err != nil || job == nil || job.AgentID != id {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "job not found for this agent"})
		return
	}
	job.Enabled = req.Enabled
	if err := s.dataStore.SaveCronJob(r.Context(), job); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true, "job": job})
}

// --- Cron Jobs ---

func (s *Server) handleListCronJobs(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}

	var jobs []map[string]any
	for i, job := range cfg.CronJobs {
		jobs = append(jobs, map[string]any{
			"id":       fmt.Sprintf("%d", i),
			"name":     job.Name,
			"type":     job.Type,
			"schedule": job.Schedule,
			"agentId":  job.AgentID,
			"channel":  job.Channel,
			"chatId":   job.ChatID,
			"message":  job.Message,
			"enabled":  true,
		})
	}
	if jobs == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	jsonResponse(w, http.StatusOK, jobs)
}

func (s *Server) handleCreateCronJob(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Type     string `json:"type"`
		Schedule string `json:"schedule"`
		AgentID  string `json:"agentId"`
		Channel  string `json:"channel"`
		ChatID   string `json:"chatId"`
		Message  string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	cfg.CronJobs = append(cfg.CronJobs, config.CronJob{
		Name:     req.Name,
		Type:     req.Type,
		Schedule: req.Schedule,
		AgentID:  req.AgentID,
		Channel:  req.Channel,
		ChatID:   req.ChatID,
		Message:  req.Message,
	})

	if err := s.saveUserConfig(r, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateCronJob(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	var idx int
	if _, err := fmt.Sscanf(idStr, "%d", &idx); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid id"})
		return
	}

	var req struct {
		Enabled *bool `json:"enabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid request"})
		return
	}

	// For now, just acknowledge — cron enable/disable would need scheduler integration
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteCronJob(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	var idx int
	if _, err := fmt.Sscanf(idStr, "%d", &idx); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid id"})
		return
	}

	cfg, err := s.loadUserConfig(r)
	if err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	if idx < 0 || idx >= len(cfg.CronJobs) {
		jsonResponse(w, http.StatusNotFound, map[string]any{"ok": false, "error": "job not found"})
		return
	}

	cfg.CronJobs = append(cfg.CronJobs[:idx], cfg.CronJobs[idx+1:]...)

	if err := s.saveUserConfig(r, cfg); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
