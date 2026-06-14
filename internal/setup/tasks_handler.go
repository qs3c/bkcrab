package setup

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qs3c/bkclaw/internal/taskqueue"
)

// TasksHandler 负责管理端的任务队列列表展示。
type TasksHandler struct {
	taskQueue *taskqueue.Queue
	mw        *Middleware
}

// NewTasksHandler 构造 TasksHandler。
func NewTasksHandler(taskQueue *taskqueue.Queue, mw *Middleware) *TasksHandler {
	return &TasksHandler{taskQueue: taskQueue, mw: mw}
}

// RegisterRoutes 注册任务队列管理路由（仅超级管理员）。
func (s *TasksHandler) RegisterRoutes(r *gin.Engine) {
	r.GET("/api/tasks", wrap(s.mw.Admin(s.handleListTasks)))
}

// --- /api/tasks ---

func (s *TasksHandler) handleListTasks(w http.ResponseWriter, r *http.Request) {
	if s.taskQueue == nil {
		jsonResponse(w, http.StatusOK, []any{})
		return
	}
	tasks := s.taskQueue.RecentTasks(50)
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		entry := map[string]any{
			"id":        t.ID,
			"agentId":   t.AgentID,
			"chatKey":   t.ChatKey,
			"status":    string(t.Status),
			"createdAt": t.CreatedAt.Format(time.RFC3339),
		}
		if t.StartedAt != nil && t.DoneAt != nil {
			entry["duration"] = t.DoneAt.Sub(*t.StartedAt).Milliseconds()
		}
		if t.Error != nil {
			entry["error"] = t.Error.Error()
		}
		out = append(out, entry)
	}
	jsonResponse(w, http.StatusOK, out)
}
