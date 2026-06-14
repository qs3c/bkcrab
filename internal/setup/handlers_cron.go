package setup

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/store"
)

// --- 按 agent 的 cron 任务（数据库支持） ---
//
// 下面传统的 /api/cron 从用户的扁平 bkclaw.json (cfg.CronJobs) 中读取任务 —
// 那是安装时静态配置的目录。通过 create_cron_job 工具在运行时调度工作的 agent
// 改为持久化到 cron_jobs 数据库表中，而 cron.Scheduler（实际触发它们的调度器）
// 只监听数据库。因此那些 agent 编写的任务对仪表板不可见。
// handleListAgentCronJobs 在 /api/agents/{id}/cron 上显示它们。

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
	// 在删除前验证任务属于此 agent — 否则路径参数可能被用来删除调用者不拥有的任务
	//（cron 表没有 user_id；我们通过 agent 拥有者进行门控）。
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

// --- Cron 任务 ---

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

	// 目前仅确认 — cron 启用/禁用需要调度器集成
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
