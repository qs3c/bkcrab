package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/agent"
	"github.com/qs3c/bkcrab/internal/auth"
	"github.com/qs3c/bkcrab/internal/bus"
)

// chatCompletionRequest 映射 OpenAI 聊天补全请求。
//
// User 是 OpenAI 标准的"终端用户标识符"字段。当请求使用
// api_key 认证时，非空值会触发将请求身份重新绑定到以
// (apikey_id, user) 为键的 bkcrab app_user，从而使会话
// 和 agent_files 按终端用户分区。偏好仅使用 header 的客户端
// 可以改用 X-Bkcrab-End-User — 两者最终走同一代码路径。
type chatCompletionRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   *bool         `json:"stream,omitempty"`
	User     string        `json:"user,omitempty"`
	// AgentID 是 bkcrab 扩展：允许调用者在请求体中选择 agent，
	// 而不是（或除了）`x-bkcrab-agent-id` 头。当两者都设置时，
	// 请求体优先 — 与 `user` 使用的模式一致。可选。
	AgentID string `json:"agent_id,omitempty"`
	// Params 是 bkcrab 扩展：调用应用随聊天提交的自由格式
	// 结构化参数块。渲染为每轮系统消息，使 agent 的 LLM
	// 在调用工具时可以遵循（例如，第三方应用的"模型选择器" +
	// "设置" UI 转换为此处的 {provider, aspect_ratio, n}，
	// 而非用户在提示中输入这些内容）。范围为每次请求 —
	// 参数不会跨轮持久化。不了解此字段的 OpenAI 客户端不受影响（omitempty）。
	Params map[string]any `json:"params,omitempty"`
	// Images 是 bkcrab 扩展：当前轮次的图片附件。
	// 每个条目为以下之一：
	//   - HTTPS URL: "https://example.com/photo.jpg"（必须可从
	//     LLM provider 访问；此处不做验证）
	//   - Data URL:  "data:image/png;base64,iVBORw0KGgo..."
	//
	// 接受的 MIME 类型取决于 LLM 模型。Anthropic / OpenAI
	// 视觉模型均支持 png、jpeg、webp；gif 则不一定。
	// 单图和总请求大小限制也取决于模型侧
	//（Anthropic ~5MB/图，OpenAI ~20MB）— bkcrab 不强制
	// 自己的上限，由上游 provider 返回拒绝。
	Images []string `json:"images,omitempty"`
	// ImageURLs 是 Images 的可接受别名。面向 Web 的聊天端点
	// 历史上称此字段为 `imageUrls`；允许在此处使用意味着
	// 为两个端点编写一个客户端的调用者在选错名称时
	// 不会导致附件被静默丢弃。
	ImageURLs []string `json:"imageUrls,omitempty"`
	// Attachments 是类型化的通用附件字段。每个条目可以携带
	// 可选的 Name，经清理后用作磁盘文件名，使 LLM 看到
	// `report.pdf` 而非 `image_3jk7l_0.pdf`。与 Images / ImageURLs
	// 不同，此处的条目不会作为视觉内容部分内联 — 它们只存入
	// /workspace 并通过 `[Attached: /workspace/X]` 面包屑到达
	// LLM。当希望字节直接显示给视觉模型时，应使用 Images /
	// ImageURLs（而非 Attachments）。
	Attachments []attachmentRequest `json:"attachments,omitempty"`
}

// attachmentRequest 是单个附件的传输格式。
type attachmentRequest struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
}

// allAttachments 将三种输入形式（Images、ImageURLs、Attachments）
// 扁平化为一个有序切片，用于物化到 /workspace。客户端通常选择
// 其中一种；也允许混合使用。
func (r chatCompletionRequest) allAttachments() []agent.Attachment {
	n := len(r.Images) + len(r.ImageURLs) + len(r.Attachments)
	if n == 0 {
		return nil
	}
	out := make([]agent.Attachment, 0, n)
	for _, u := range r.Images {
		out = append(out, agent.Attachment{URL: u})
	}
	for _, u := range r.ImageURLs {
		out = append(out, agent.Attachment{URL: u})
	}
	for _, a := range r.Attachments {
		out = append(out, agent.Attachment{URL: a.URL, Name: a.Name})
	}
	return out
}

// inlineImageURLs 仅返回符合内联视觉条件的 URL
// （PhotoURLs → image_url 内容块）。仅 Images 和 ImageURLs
// 符合条件 — 按约定它们是调用者断言为图片的。通用的
// Attachments 字段被排除：通过视觉通道传入 PDF/zip URL 会
// 导致上游 provider 返回 HTTP 400 并使整个轮次失败。
// Attachments 通过 `[Attached: /workspace/<file>]` 面包屑
// 到达 LLM。
func (r chatCompletionRequest) inlineImageURLs() []string {
	if len(r.Images) == 0 && len(r.ImageURLs) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.Images)+len(r.ImageURLs))
	out = append(out, r.Images...)
	out = append(out, r.ImageURLs...)
	return out
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatCompletionChunk 是流式模式下单个 SSE 数据块。
type chatCompletionChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// chatCompletionResponse 是非流式响应。
type chatCompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   completionUsage    `json:"usage"`
}

type completionChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type completionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// HandleChatCompletions 处理 POST /v1/chat/completions。
func (s *Server) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	var req chatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "invalid request body", "type": "invalid_request_error"},
		})
		return
	}

	if len(req.Messages) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "messages is required", "type": "invalid_request_error"},
		})
		return
	}

	// OpenAI 的 `user` 请求体字段，在 api_key 调用中出现时，
	// 会将身份重新绑定到对应的 app_user（懒创建）。
	// Header X-Bkcrab-End-User 在认证中间件中的预处理阶段完成
	// 相同的工作；我们在中间件*之后*运行此逻辑，因此当两者
	// 同时存在时请求体值优先（请求体字段比静态 header 更具体
	// 地针对本次调用）。此处的错误是非致命的 — 请求在未切换的
	// 身份下继续进行。
	if req.User != "" && s.authResolver != nil {
		if ident, ok := auth.FromContext(r.Context()); ok {
			if next, swErr := s.authResolver.SwitchToAppUser(r.Context(), ident, req.User); swErr == nil {
				r = r.WithContext(auth.WithIdentity(r.Context(), next))
			}
		}
	}

	// 解析调用者的用户空间（由 authMiddleware 设置）并从中选取一个 agent。
	space, err := s.userSpaceFor(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": map[string]string{"message": err.Error(), "type": "authentication_error"},
		})
		return
	}

	// 请求体字段优先于 header — 与 `user` 的优先级一致。
	// 让应用调用者可以在一个 JSON 中发送所有内容，无需处理 header。
	agentID := r.Header.Get("x-bkcrab-agent-id")
	if req.AgentID != "" {
		agentID = req.AgentID
	}
	ag := resolveAgent(space, agentID)
	if ag == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]string{"message": "agent not found", "type": "not_found_error"},
		})
		return
	}
	// Apikey ACL 门控。UserSpaceFor 加载所有者拥有的每个 agent，
	// 无论此特定 apikey 限定到哪个子集。无此检查时，限定到
	// 一个 agent 的 type=agent apikey 可以传入
	// `x-bkcrab-agent-id: <同级>`（或省略并回退到 default / all[0]）
	// 并与所有者的任何 agent 通信。/v1/agents 列表已经通过
	// CanAccessAgent 进行过滤 — 在此镜像该逻辑以便统一执行
	// apikey 范围。使用 404（而非 403），使响应与真正的
	// "无此 agent" 情况一致，ACL 不会泄露超出范围的 agent 的存在。
	if ident, ok := auth.FromContext(r.Context()); ok && !ident.CanAccessAgent(ag.Name()) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": map[string]string{"message": "agent not found", "type": "not_found_error"},
		})
		return
	}

	// 从 header 构建会话密钥
	sessionKey := r.Header.Get("x-bkcrab-session-key")
	if sessionKey == "" {
		sessionKey = "api-" + fmt.Sprintf("%d", time.Now().UnixNano())
	}

	// 提取最后一条用户消息
	var userText string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			userText = req.Messages[i].Content
			break
		}
	}
	if userText == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": map[string]string{"message": "no user message found", "type": "invalid_request_error"},
		})
		return
	}

	// 将附件图片物化到 agent 的会话工作区，并在前面添加与 Web UI
	// 使用的相同的 `[Attached: /workspace/<file>]` 面包屑
	// (web/src/app/agents/[id]/chat/page.tsx:639-645)，使 Web 和
	// API 入口的传输格式一致。冗长的"请勿探测"提示反而适得其反 —
	// 当提示中强调路径时，模型会本能地运行 which/ls/file 来"验证"路径。
	// PhotoURLs 被保留，以便视觉 LLM 仍能内联看到图片。
	// API 客户端目前无法寻址项目 — 聊天补全仅知道 session_key —
	// 因此附件始终落入松散聊天范围。当/如果我们在此暴露项目寻址时，
	// 查找会话行并传入其 project_id 而非 ""。
	atts := req.allAttachments()
	attachmentPaths := ag.WriteSessionAttachments(r.Context(), sessionKey, "", atts)
	if len(attachmentPaths) > 0 {
		var b strings.Builder
		for _, p := range attachmentPaths {
			b.WriteString("[Attached: /workspace/")
			b.WriteString(p)
			b.WriteString("]\n")
		}
		b.WriteString(userText)
		userText = b.String()
	}

	// 构建入站消息。
	// X-Bkcrab-Channel 允许调用者覆盖回复通道，使此轮创建的
	// 定时任务通过正确的适配器路由（例如 "pinclaw" → plugin
	// channel.send → Cloud API）。
	channel := r.Header.Get("x-bkcrab-channel")
	if channel == "" {
		channel = "api"
	}
	msg := bus.InboundMessage{
		Channel:   channel,
		ChatID:    sessionKey,
		UserID:    "api-user",
		Text:      userText,
		PeerKind:  "dm",
		Params:    req.Params,
		PhotoURLs: req.inlineImageURLs(),
	}

	slog.Info("chat completion request",
		"agent", ag.Name(),
		"session", sessionKey,
		"stream", req.Stream != nil && *req.Stream,
	)

	model := ag.Model()
	if req.Model != "" {
		model = req.Model
	}
	chatID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	now := time.Now().Unix()

	isStream := req.Stream != nil && *req.Stream
	if isStream {
		s.streamResponseFromAgent(w, r, ag, msg, chatID, model, now)
	} else {
		// 从 agent 获取回复
		reply := ag.HandleMessage(r.Context(), msg)
		s.fullResponse(w, reply, chatID, model, now)
	}
}

func (s *Server) streamResponseFromAgent(w http.ResponseWriter, r *http.Request, ag *agent.Agent, msg bus.InboundMessage, chatID, model string, created int64) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)

	sr := ag.HandleMessageStream(r.Context(), msg)

	// 发送角色数据块
	s.writeSSEChunk(w, chatID, model, created, "assistant", "", nil)
	if ok {
		flusher.Flush()
	}

	// 转发 StreamReader 的数据块
	for {
		chunk, more := sr.Next()
		if chunk.Content != "" {
			s.writeSSEChunk(w, chatID, model, created, "", chunk.Content, nil)
			if ok {
				flusher.Flush()
			}
		}
		if chunk.Done || !more {
			break
		}
	}

	// 发送完成数据块
	done := "stop"
	s.writeSSEChunk(w, chatID, model, created, "", "", &done)
	fmt.Fprint(w, "data: [DONE]\n\n")
	if ok {
		flusher.Flush()
	}
}

func (s *Server) writeSSEChunk(w http.ResponseWriter, id, model string, created int64, role, content string, finishReason *string) {
	chunk := chatCompletionChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: created,
		Model:   model,
		Choices: []chunkChoice{
			{
				Index: 0,
				Delta: chunkDelta{
					Role:    role,
					Content: content,
				},
				FinishReason: finishReason,
			},
		},
	}
	data, _ := json.Marshal(chunk)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func (s *Server) fullResponse(w http.ResponseWriter, reply, chatID, model string, created int64) {
	resp := chatCompletionResponse{
		ID:      chatID,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []completionChoice{
			{
				Index:        0,
				Message:      chatMessage{Role: "assistant", Content: reply},
				FinishReason: "stop",
			},
		},
		Usage: completionUsage{
			PromptTokens:     0,
			CompletionTokens: 0,
			TotalTokens:      0,
		},
	}
	writeJSON(w, http.StatusOK, resp)
}

// resolveAgent 从调用者的用户空间中选取一个 agent，优先使用
// x-bkcrab-agent-id 头中的显式 agent ID，回退到默认/第一个 agent。
func resolveAgent(space *UserSpaceView, agentID string) *agent.Agent {
	mgr := space.Agents
	if agentID != "" {
		if ag := mgr.AgentByID(agentID); ag != nil {
			return ag
		}
	}
	if def := mgr.DefaultAgent(); def != nil {
		return def
	}
	all := mgr.All()
	if len(all) > 0 {
		return all[0]
	}
	return nil
}
