package setup

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/qs3c/bkclaw/internal/store"
)

// 项目是按 (user, agent) 命名的工作区文件夹，用于组织聊天会话。
// 同一项目中的每个聊天共享一个工作区目录 workspaces/<agent>/projects/<pid>/，
// 因此笔记/生成的文件在项目的聊天之间持久存在 — 这就是此功能的核心目的。
//
// 下面暴露的端点：
//
//	GET    /api/agents/{id}/projects                   — 列出（调用者自己的）
//	POST   /api/agents/{id}/projects                   — 创建
//	PATCH  /api/agents/{id}/projects/{pid}             — 重命名 / 重新描述
//	DELETE /api/agents/{id}/projects/{pid}             — 删除（当还有聊天时阻止）
//
// 项目聊天是延迟创建的：在侧边栏中点击"在项目中新建聊天"只是导航到 `/agents/<id>/chat/?project=<pid>`，
// 第一个用户消息在聊天请求体中携带 `projectId`。从那里触发的第一个 SaveSession
// 用 project_id 标记新会话行；后续保存不触碰它（SQL upsert 中的 ON CONFLICT 保持它）。
// 没有预创建端点 — 防止"用户打开了新建聊天然后离开"导致侧边栏充满空行。

// generateProjectID 生成一个不透明的 project_id，匹配 users (u_) 和 agents (agt_)
// 使用的 `<prefix>_<hex20>` 格式，使 ID 在整个平台上视觉一致。约 80 位熵 —
// 在平台规模上抗碰撞；我们不会探测存储的唯一性，因为调用者已经通过了 requireAgentOwner。
func generateProjectID() string {
	var buf [10]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand 在支持的平台上不应失败；如果失败，则返回一个可识别的哨兵值，
		// 而不是静默生成 "proj_"（它会在并发调用中与自身冲突）。
		return "proj_rngerror"
	}
	return "proj_" + hex.EncodeToString(buf[:])
}

// trimProjectName 去除首尾空白并限制长度，以免恶意的 64KB 内容出现在侧边栏中。
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

func (s *ProjectsHandler) handleListProjects(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	uid := effectiveUserID(r)
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

func (s *ProjectsHandler) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	// 可读即可（不必是拥有者）：项目以 (user_id, agent_id, project_id) 为键，
	// 因此在共享 agent 上创建项目的查看者只会添加到自己 user_id 下的行，永远不会影响拥有者的项目列表。
	// 下面的更新/删除同理。
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	uid := effectiveUserID(r)
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
	// 重新读取以获取数据库在行上标记的权威 created_at / updated_at，而不是返回零值时间。
	saved, err := s.dataStore.GetProject(r.Context(), uid, id, rec.ID)
	if err != nil || saved == nil {
		// 回退到内存中的副本 — ID 和名称是正确的，只是时间戳为零。
		// 仍然比成功插入后给调用者返回 500 好。
		jsonResponse(w, http.StatusOK, projectToJSON(rec))
		return
	}
	jsonResponse(w, http.StatusOK, projectToJSON(saved))
}

func (s *ProjectsHandler) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	if !requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	pid := r.PathValue("pid")
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	uid := effectiveUserID(r)
	existing, err := s.dataStore.GetProject(r.Context(), uid, id, pid)
	if err != nil || existing == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "project not found"})
		return
	}
	// PATCH 语义：仅更新调用者发送的字段。
	// 使用指针以便区分"未发送"和"发送了空字符串" — 后者是合法的清空描述操作。
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

func (s *ProjectsHandler) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	if !requireWritable(w, r) {
		return
	}
	id := r.PathValue("id")
	pid := r.PathValue("pid")
	if !s.guard.requireAgentReadable(w, r, id) {
		return
	}
	uid := effectiveUserID(r)
	// 拒绝删除仍拥有聊天的项目。级联/软分离被故意不暴露 —
	// v1 将破坏性操作放在显式的"先删除聊天"步骤之后，
	// 这样在垃圾桶图标上的一次误操作不会清除相当于一份调查报告的笔记。
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
