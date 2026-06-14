package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/qs3c/bkclaw/internal/bus"
)

// AgentHandler 处理消息并返回响应。
type AgentHandler interface {
	HandleMessage(ctx context.Context, agentID string, msg bus.InboundMessage) (string, error)
}

// UserLookup 将 bearer token 解析为用户 ID（云模式）。
type UserLookup interface {
	LookupByToken(token string) (string, bool)
}

// WebhookRequest 是 webhook POST 请求的请求体。
type WebhookRequest struct {
	AgentID string `json:"agentId"`
	UserID  string `json:"userId,omitempty"` // bkclaw user to route to (cloud mode)
	Message string `json:"message"`
	Channel string `json:"channel"`
	ChatID  string `json:"chatId"`
}

// WebhookResponse 是返回给 webhook 调用者的 JSON 响应。
type WebhookResponse struct {
	OK      bool   `json:"ok"`
	Reply   string `json:"reply,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Server 是 webhook HTTP 服务器。
type Server struct {
	token      string
	path       string
	handler    AgentHandler
	userLookup UserLookup // optional (nil in local mode)
}

// NewServer 创建一个新的 webhook 服务器。在本地模式下 userLookup 可为 nil。
func NewServer(token, path string, handler AgentHandler, userLookup UserLookup) *Server {
	if path == "" {
		path = "/hooks"
	}
	return &Server{
		token:      token,
		path:       path,
		handler:    handler,
		userLookup: userLookup,
	}
}

// SetHandler 替换代理处理程序。被 gateway.New 使用，以便处理程序
// 可以持有正在构建的 Gateway 的引用，而无需解决先有鸡还是先有蛋的问题。
func (s *Server) SetHandler(h AgentHandler) {
	s.handler = h
}

// Handler 返回 webhook 端点的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(s.path, s.handleWebhook)
	return mux
}

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, WebhookResponse{Error: "method not allowed"})
		return
	}

	// 验证 bearer token 并可选择解析为用户 ID。
	var ownerUserID string
	auth := r.Header.Get("Authorization")
	token := strings.TrimPrefix(auth, "Bearer ")
	if s.token != "" {
		if token == s.token {
			// 管理员/本地模式 token 匹配。
		} else if s.userLookup != nil {
			if uid, ok := s.userLookup.LookupByToken(token); ok {
				ownerUserID = uid
			} else {
				writeJSON(w, http.StatusUnauthorized, WebhookResponse{Error: "unauthorized"})
				return
			}
		} else {
			writeJSON(w, http.StatusUnauthorized, WebhookResponse{Error: "unauthorized"})
			return
		}
	}

	var req WebhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, WebhookResponse{Error: "invalid request body"})
		return
	}

	if req.AgentID == "" {
		writeJSON(w, http.StatusBadRequest, WebhookResponse{Error: "agentId is required"})
		return
	}
	if req.Message == "" {
		writeJSON(w, http.StatusBadRequest, WebhookResponse{Error: "message is required"})
		return
	}

	channel := req.Channel
	if channel == "" {
		channel = "webhook"
	}
	chatID := req.ChatID
	if chatID == "" {
		chatID = "webhook-default"
	}

	// 优先使用请求体中显式的 userId，然后是 token 派生的。
	if req.UserID != "" {
		ownerUserID = req.UserID
	}

	msg := bus.InboundMessage{
		Channel:     channel,
		ChatID:      chatID,
		UserID:      "webhook",
		OwnerUserID: ownerUserID,
		Text:        req.Message,
		PeerKind:    "dm",
	}

	slog.Info("webhook received",
		"agent", req.AgentID,
		"channel", channel,
		"chat_id", chatID,
	)

	reply, err := s.handler.HandleMessage(r.Context(), req.AgentID, msg)
	if err != nil {
		slog.Error("webhook handler error", "agent", req.AgentID, "error", err)
		writeJSON(w, http.StatusInternalServerError, WebhookResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, WebhookResponse{OK: true, Reply: reply})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ListenAndServe 在给定地址上启动 webhook 服务器。
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
	}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	slog.Info("webhook server started", "addr", addr, "path", s.path)
	err := srv.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return fmt.Errorf("webhook server: %w", err)
}
