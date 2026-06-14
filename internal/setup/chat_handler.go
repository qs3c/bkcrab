package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qs3c/bkclaw/internal/agent"
	"github.com/qs3c/bkclaw/internal/bus"
	"github.com/qs3c/bkclaw/internal/channels"
	"github.com/qs3c/bkclaw/internal/session"
	"github.com/qs3c/bkclaw/internal/store"
	"github.com/qs3c/bkclaw/internal/workspace"
)

// ChatHandler 负责 web 聊天界面 — 轮次、引导(steer)、流式、SSE 订阅、历史与会话管理。
type ChatHandler struct {
	dataStore  store.Store
	webChan    *channels.WebChannel
	chatEvents *agent.EventHub
	guard      *agentGuard
	ws         *workspaceRepo
	mw         *Middleware
}

// NewChatHandler 构造 ChatHandler。
func NewChatHandler(dataStore store.Store, webChan *channels.WebChannel, chatEvents *agent.EventHub, guard *agentGuard, ws *workspaceRepo, mw *Middleware) *ChatHandler {
	return &ChatHandler{dataStore: dataStore, webChan: webChan, chatEvents: chatEvents, guard: guard, ws: ws, mw: mw}
}

// RegisterRoutes 注册 web 聊天相关路由。
func (s *ChatHandler) RegisterRoutes(r *gin.Engine) {
	r.POST("/api/chat", wrap(s.mw.Auth(s.handleChat)))
	r.POST("/api/chat/stream", wrap(s.mw.Auth(s.handleChatStream)))
	r.POST("/api/chat/steer", wrap(s.mw.Auth(s.handleChatSteer)))
	r.GET("/api/chat/history", wrap(s.mw.Auth(s.handleChatHistory)))
	r.GET("/api/chat/todo", wrap(s.mw.Auth(s.handleChatTodo)))
	r.GET("/api/chat/sessions", wrap(s.mw.Auth(s.handleChatSessions)))
	r.PUT("/api/chat/sessions/:key", wrap(s.mw.Auth(s.handleRenameSession)))
	r.DELETE("/api/chat/sessions/:key", wrap(s.mw.Auth(s.handleDeleteSession)))
	r.PATCH("/api/chat/sessions/:key/project", wrap(s.mw.Auth(s.handleMoveSessionProject)))
	r.GET("/api/chat/subscribe", wrap(s.mw.Auth(s.handleChatSubscribe)))
}

type chatRequest struct {
	AgentID   string `json:"agentId,omitempty"`
	SessionID string `json:"sessionId"`
	// ProjectID，当非空且 session 行尚不存在时，
	// 是 URL 在第一消息之前携带的"此聊天属于项目 X"提示（`?project=<pid>`）。
	// 一旦行存在，它就是权威的 — 服务器从行中读取 project_id 并忽略任何后续提示。
	ProjectID string `json:"projectId,omitempty"`
	Message   string `json:"message"`
	// Images 携带用于图像附件的数据 URL / HTTPS URL。
	// Web 客户端历史上在 `imageUrls`（驼峰式）下发送它们，
	// 而 API 路径使用 `images`；我们两者都接受并在下面合并，
	// 以便服务器端管道有一个规范的切片。没有这个，
	// web 的 image_url 内容部分永远不会到达 agent（空切片
	// → 没有 ContentParts 持久化 → 历史重载不显示图像，
	// vision LLM 只看到文本面包屑）。
	Images    []string `json:"images,omitempty"`
	ImageURLs []string `json:"imageUrls,omitempty"`
	// Attachments 是类型化的通用附件字段。每个条目可以携带一个可选的 Name，
	// 该 Name 被清理并用作磁盘文件名，以便 LLM 看到 `report.pdf` 而不是
	// `image_3jk7l_0.pdf`。与 Images / ImageURLs 不同，此处的条目
	// 不会内联为 vision 内容部分 — 它们只进入 /workspace
	// 并通过 `[Attached: /workspace/X]` 面包屑到达 LLM。
	// 当你希望字节直接显示给视觉模型时，使用 Images / ImageURLs（而不是 Attachments）。
	Attachments []attachmentRequest `json:"attachments,omitempty"`
	Params      map[string]any      `json:"params,omitempty"`
}

// attachmentRequest 是单个附件的传输形式。URL 是 data: 或 http(s) URL；Name 是可选的调用者提供的文件名。
type attachmentRequest struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
}

// allAttachments 将三种输入形状（Images、ImageURLs、Attachments）
// 扁平化为一个有序切片以物化到 /workspace。
// 顺序：Images → ImageURLs → Attachments。客户端通常只选一个；允许混合且不进行去重。
func (r chatRequest) allAttachments() []agent.Attachment {
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

// inlineImageURLs 返回应内联为 vision 内容部分（PhotoURLs → image_url 内容块）的 URL。
// 只有遗留的纯图像字段符合条件：Images 和 ImageURLs 按约定是调用者断言的图像，
// 因此将它们包裹为 image_url 是安全的。较新的 Attachments 字段是通用的（pdf / zip / txt 都是合法的），
// 将非图像 URL 提供给 provider 视觉通道会导致整个轮次失败（Anthropic 对
// `{type:image, source:{type:url, url:<pdf>}}` 返回 400）。
// Attachments 通过 `[Attached: /workspace/<file>]` 面包屑到达 LLM。
func (r chatRequest) inlineImageURLs() []string {
	if len(r.Images) == 0 && len(r.ImageURLs) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.Images)+len(r.ImageURLs))
	out = append(out, r.Images...)
	out = append(out, r.ImageURLs...)
	return out
}

// preMaterialized 报告调用者是否已将附件 + 前缀 `[Attached: /workspace/...]` 面包屑
// 上传到消息中。Web 客户端端到端地执行此操作（uploadAgentFiles + chat/page.tsx 中的内联面包屑）；
// 在服务器端再次执行会以生成的名称重复写入文件并发出第二个面包屑，
// 这会使 LLM 读取为两个不同的图像并尝试分别编辑每个。
// 仅通过聊天补全扩展发送原始图像的 API 调用者没有面包屑，因此服务器必须代表他们物化。
func (r chatRequest) preMaterialized() bool {
	return strings.HasPrefix(r.Message, "[Attached:")
}

// annotateMessageWithAttachments 在每个附件前向用户消息添加一行
// `[Attached: /workspace/<file>]` — 与 Web UI 使用的相同面包屑格式
// （参见 web/src/app/agents/[id]/chat/page.tsx:639-645），
// 因此 LLM 看到的传输形状无论轮次是通过 Web 聊天还是聊天 API 到达都是相同的。
// provider.StripAttachedPrefix 在存储的历史记录到达 UI 气泡/页面标题之前清除这些标签。
//
// 我们有意识地不添加尾随的"do not probe"块。早期的一次尝试这样做了 —
// 但显式的负面指令引起了与预期相反的效果（模型反射性地
// `which`/`ls`/`file` 路径"以确认"然后才使用它）。
// Web 路径证明单个裸面包屑就足够了；精确镜像即可。
func annotateMessageWithAttachments(message string, paths []string) string {
	if len(paths) == 0 {
		return message
	}
	var b strings.Builder
	for _, p := range paths {
		b.WriteString("[Attached: /workspace/")
		b.WriteString(p)
		b.WriteString("]\n")
	}
	if message != "" {
		b.WriteString(message)
	}
	return b.String()
}

func (s *ChatHandler) handleChat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.guard.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	atts := req.allAttachments()
	msgText := req.Message
	if !req.preMaterialized() {
		// Resolve the chat's project so attachments land in
		// projects/<pid>/ when the session belongs to one. Best-effort:
		// failure → empty pid → loose-chat scope (the historical
		// behavior).
		projectID := s.ws.resolveSessionProject(r.Context(), r, ag.Name(), req.SessionID)
		paths := ag.WriteSessionAttachments(r.Context(), req.SessionID, projectID, atts)
		msgText = annotateMessageWithAttachments(req.Message, paths)
	}
	reply := ag.HandleWebChat(r.Context(), req.SessionID, req.ProjectID, effectiveUserID(r), msgText, req.inlineImageURLs(), req.Params)
	jsonResponse(w, http.StatusOK, map[string]any{"reply": reply})
}

// handleChatSteer 将一个消息缓冲到正在进行的轮次中。
// 它不会打开流或发出事件 — 正在运行的轮次（由先前的 /api/chat/stream POST 启动）
// 在工具轮次之间折叠该消息并在其现有的 SSE 上发出 "steer" 事件。
// 当有活跃轮次时返回 200 {"buffered":true}；没有运行时返回 409 {"buffered":false}，
// 以便客户端回退到普通的 /api/chat/stream 发送。
func (s *ChatHandler) handleChatSteer(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.guard.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if effectiveUserID(r) == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if strings.TrimSpace(req.Message) == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "empty message"})
		return
	}
	if ag.SteerWeb(req.SessionID, req.ProjectID, req.Message) {
		jsonResponse(w, http.StatusOK, map[string]any{"buffered": true})
		return
	}
	jsonResponse(w, http.StatusConflict, map[string]any{"buffered": false})
}

// agentTurnTimeout 是客户端连接断开后允许 agent goroutine 运行的上限。
// 在扇出 delegate_task 工作（6 个并行子 agent × 每个约 10 分钟驱动 camoufox-cli）
// 在 Chat 调用中间常规性地突破之前 15m 预算后，提升到 45m，
// 立即向所有兄弟显示"context deadline exceeded"。
// 45m 舒适地超出带有浏览器自动化的实际最大并行扇出；
// 仍然有边界，因此真正的失控循环不会永久占用 goroutine。
const agentTurnTimeout = 45 * time.Minute

func (s *ChatHandler) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	ag := s.guard.resolveAgent(r, req.AgentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	uid := effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// 击败 nginx / Cloudflare 对长寿命响应的缓冲；
	// agent 循环以人类打字速度发出块，我们希望它们立即在线路上传输，
	// 而不是保持到响应关闭。
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()
	atts := req.allAttachments()
	imageURLs := req.inlineImageURLs()
	msgText := req.Message
	if !req.preMaterialized() {
		projectID := s.ws.resolveSessionProject(r.Context(), r, ag.Name(), req.SessionID)
		paths := ag.WriteSessionAttachments(r.Context(), req.SessionID, projectID, atts)
		msgText = annotateMessageWithAttachments(req.Message, paths)
	}

	// 在启动 agent 之前订阅 hub，这样我们就不会与第一个发出的事件竞争。
	// hub 缓冲进行中的事件，因此 emitEvent 的分发即使我们消耗缓慢也永远不会阻塞。
	hub := s.chatEvents
	agentID := ag.Name()
	sub, unsubscribe := hub.Subscribe(uid, agentID, req.SessionID)
	defer unsubscribe()

	// 从请求中分离 agent 的 ctx：当浏览器标签页断开连接（刷新、关闭、网络抖动）时，
	// 我们希望 agent 继续运行，以便其已付费的 LLM 调用完成并且回复记录在 session_events 中。
	// 15 分钟的上限是唯一可以杀死它的因素。
	agentCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), agentTurnTimeout)
	// cancel 活在 handler 上，而不是 agent goroutine 上：当斜杠命令
	// 排队一个延续时，我们在 HandleMessage 返回之后保持 SSE 打开，
	// 而内部作用域的 cancel 会在延续的事件到达此 handler 的安全网检查之前拆除 agentCtx。
	defer cancel()
	agentCtx = agent.ContextWithStream(agentCtx, nil, s.dataStore, hub, uid, agentID, req.SessionID)

	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		// events 参数保持为 nil — emitEvent 现在通过上面附加的
		// streamCtx 扇出（persist + hub）。此 handler 不再需要旧的通道路径。
		_ = ag.HandleWebChatStream(agentCtx, req.SessionID, req.ProjectID, uid, msgText, imageURLs, req.Params, nil)
	}()

	// Heartbeat 防止代理（nginx 60s 默认、Cloudflare 100s、ELB 60s）
	// 在 agent 正在思考但尚未发出内容时杀死空闲的 SSE 连接。
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	clientGone := r.Context().Done()
	// turnPending 在斜杠命令 handler 报告它通过 bus.Inbound 排队了
	// 一个延续（`turn_pending` 事件）时打开。
	// POST goroutine 的 HandleMessage 已经返回，但真正的回复仍在
	// 不同的 goroutine 上 10-15 秒外 — 我们保持 SSE 打开，
	// 以便浏览器的打字指示器保持可见，延续的 content_delta/content 事件
	// 流入同一个连接。当延续自己的 `done` 到达时清除，
	// 此时循环正常返回。
	turnPending := false
	for {
		select {
		case <-clientGone:
			// 客户端断开；agent goroutine 在其分离的 ctx 上继续运行
			// 并持久化它发出的每个事件。重新加载聊天页面的用户
			// 将通过 /api/chat/subscribe?since=N 获取其余内容。
			return
		case <-agentDone:
			// 竞争：HandleMessage 向 hub 发布 `turn_pending`
			// 并且 `defer close(agentDone)` 从同一个 goroutine 触发。
			// 当两者都就绪时，Go 的 select 随机选择，因此即使
			// turn_pending 事件在 sub 缓冲区中，agentDone 也可能获胜。
			// 首先排空待处理事件以使决策确定性。
		drain:
			for {
				select {
				case env, ok := <-sub:
					if !ok {
						return
					}
					if env.Event.Type == "turn_pending" {
						turnPending = true
						continue
					}
					if env.Event.Type == "done" {
						return
					}
					forwardEvent(w, flusher, env)
				default:
					break drain
				}
			}
			if turnPending {
				// HandleMessage 在排队一个延续后静默返回。
				// 不要关闭；等待延续的 `done` 事件通过 hub。
				// agentCtx.Done()（15 分钟超时）如果它永远不到达则是上限。
				agentDone = nil
				continue
			}
			return
		case <-agentCtx.Done():
			// 上面 turnPending 路径的安全网：
			// 即使没有 `done` 事件到达，也在 agent 上下文硬超时时退出。
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case env, ok := <-sub:
			if !ok {
				return
			}
			if env.Event.Type == "turn_pending" {
				turnPending = true
				continue
			}
			forwardEvent(w, flusher, env)
			if env.Event.Type == "done" {
				return
			}
		}
	}
}

// forwardEvent 将一个 EventEnvelope 写入 SSE 响应。
// 在 JSON 负载中包含 seq 内联（除了 SSE `id:` 行），
// 以便前端 POST sendChatStream 使用的基于 fetch 的解析器
// 可以针对并行的 /api/chat/subscribe SSE 连接进行去重。
// 没有这个去重，每个块在活跃轮次期间会渲染两次。
func forwardEvent(w http.ResponseWriter, flusher http.Flusher, env agent.EventEnvelope) {
	payload := map[string]any{
		"seq":  env.Seq,
		"type": env.Event.Type,
	}
	if env.Event.Data != nil {
		payload["data"] = env.Event.Data
	}
	data, _ := json.Marshal(payload)
	if env.Seq >= 0 {
		fmt.Fprintf(w, "id: %d\n", env.Seq)
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
}

// handleChatSubscribe 为一对（agent, session）保持一个 SSE 连接打开，
// 并转发三种类型的流量：
//
//  1. Replay：seq > since（或 > Last-Event-ID）的 session_events 行，
//     客户端在连接之前错过了。让刚重新加载的页面拾取进行中的轮次，而无需回复的其余部分消失。
//
//  2. 来自 hub 的实时 agent 聊天事件 — agent 循环中的每个 emitEvent 调用
//     都通过这里扇出。这涵盖同步 POST /api/chat/stream 路径
//     以及其他标签页/cron 触发启动的轮次，因此任何打开的聊天面板都能看到它们，
//     无论谁触发了工作。
//
//  3. 遗留的 WebChannel bus 消息 — 通过 bus.Outbound 路由的 cron 触发最终回复，
//     而不是聊天事件路径。保留以免我们在过渡期间丢失现有的功能。
//
// Auth 门控重用 resolveAgent，因此调用者必须已经有权限与此 agent 聊天。
// 订阅本身不会产生任何流量 — 关闭是静默的（客户端离开）。
func (s *ChatHandler) handleChatSubscribe(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	sessionID := r.URL.Query().Get("sessionId")
	if agentID == "" || sessionID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agentId and sessionId required"})
		return
	}
	if ag := s.guard.resolveAgent(r, agentID); ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	uid := effectiveUserID(r)
	if uid == "" {
		jsonResponse(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": "streaming unsupported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// 初始刷新以便客户端 EventSource 立即触发 `open`。
	fmt.Fprintf(w, ": ok\n\n")
	flusher.Flush()

	// 恢复点：优先使用 Last-Event-ID（浏览器管理的重连），
	// 回退到显式传递它的调用者的 ?since=N。 -1 表示"仅实时流，无回放"。
	sinceSeq := int64(-1)
	if hdr := r.Header.Get("Last-Event-ID"); hdr != "" {
		if v, err := strconv.ParseInt(hdr, 10, 64); err == nil {
			sinceSeq = v
		}
	}
	if q := r.URL.Query().Get("since"); q != "" {
		if v, err := strconv.ParseInt(q, 10, 64); err == nil {
			sinceSeq = v
		}
	}

	hub := s.chatEvents
	// 在回放之前订阅，这样在我们扫描数据库时到达的任何事件
	// 最终要么在回放范围内，要么在实时通道中 — 永远不会两者都，永远不会丢失。
	live, unsubscribeLive := hub.Subscribe(uid, agentID, sessionID)
	defer unsubscribeLive()

	// 从持久化日志中回放错过的事件。
	if s.dataStore != nil {
		rows, err := s.dataStore.ListSessionEventsSince(r.Context(), uid, agentID, sessionID, sinceSeq)
		if err != nil {
			slog.Warn("session_events replay failed", "agent", agentID, "session", sessionID, "since", sinceSeq, "error", err)
		}
		for _, rec := range rows {
			fmt.Fprintf(w, "id: %d\n", rec.Seq)
			if len(rec.Data) == 0 || string(rec.Data) == "null" {
				fmt.Fprintf(w, "data: {\"seq\":%d,\"type\":%q}\n\n", rec.Seq, rec.Type)
			} else {
				fmt.Fprintf(w, "data: {\"seq\":%d,\"type\":%q,\"data\":%s}\n\n", rec.Seq, rec.Type, string(rec.Data))
			}
			flusher.Flush()
			if rec.Seq > sinceSeq {
				sinceSeq = rec.Seq
			}
		}
	}

	// 遗留的 webChan 路径：cron 触发的 bus.Outbound 消息。保留直到
	// cron 路径被重构为通过聊天事件 hub 发出（然后这可以消失）。
	var outbound <-chan bus.OutboundMessage
	var unsubscribeOutbound func() = func() {}
	if s.webChan != nil {
		outbound, unsubscribeOutbound = s.webChan.Subscribe(agentID, sessionID)
	}
	defer unsubscribeOutbound()

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-keepalive.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case env, ok := <-live:
			if !ok {
				return
			}
			// content_delta 是驱动活跃轮次气泡的高吞吐量逐 token 流。
			// 它有意识地被 NOT 持久化（参见 emitEvent），以 seq=-1 到达，
			// 并且已经通过同一 hub 上的 POST /api/chat/stream 订阅传递给了发起标签页。
			// 在这里转发它会在活跃标签页上双倍渲染；中途加入的重新加载者
			// 会错过部分揭示但仍然获得包含完整文本的尾随 `content` 事件。
			if env.Event.Type == "content_delta" {
				continue
			}
			// 丢弃回放重叠事件：任何 seq <= 我们在回放期间已经流式传输的最高 seq 的事件。
			// 没有这个，在完全错误的时刻重新连接的浏览器会渲染相同的内容块两次。
			if env.Seq >= 0 && env.Seq <= sinceSeq {
				continue
			}
			if env.Seq >= 0 {
				sinceSeq = env.Seq
				fmt.Fprintf(w, "id: %d\n", env.Seq)
			}
			payload := map[string]any{
				"seq":  env.Seq,
				"type": env.Event.Type,
			}
			if env.Event.Data != nil {
				payload["data"] = env.Event.Data
			}
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case msg, ok := <-outbound:
			if !ok {
				outbound = nil
				continue
			}
			payload := map[string]any{
				"text":      msg.Text,
				"parseMode": msg.ParseMode,
			}
			if len(msg.MediaItems) > 0 {
				items := make([]map[string]any, 0, len(msg.MediaItems))
				for _, m := range msg.MediaItems {
					items = append(items, map[string]any{
						"filename":    m.Filename,
						"contentType": m.ContentType,
					})
				}
				payload["mediaItems"] = items
			}
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleChatTodo 读取 agent 维护的每个会话 todo.md，
// 并同时以原始 markdown 和解析的检查列表形式返回。
// 我们在这里解析 session key → (chatID, projectID)，
// 以便前端不需要知道磁盘路径布局（`sessions/<chat>/todo.md` vs `projects/<pid>/<chat>/todo.md`）。
//
// 缺少的文件不是错误 — 不使用 todo 约定的新会话或运行返回 {items: [], raw: ""}。
// 当 items 为空时前端隐藏面板。
func (s *ChatHandler) handleChatTodo(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	sessionID := r.URL.Query().Get("sessionId")
	ag := s.guard.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if sessionID == "" {
		jsonResponse(w, http.StatusOK, map[string]any{"items": []any{}, "raw": ""})
		return
	}

	// Build the agent-relative path. Project chats live under
	// projects/<pid>/<chat>/, plain chats under sessions/<chat>/. The
	// agent's workdir resolves bare filenames to its session subdir, so
	// a `write_file("todo.md", ...)` from the agent lands at one of
	// these two paths — same shape that handleAgentFileList already
	// surfaces.
	chatID := s.ws.workspaceSessionScope(r.Context(), ag.Name(), sessionID)
	projectID := s.ws.resolveSessionProject(r.Context(), r, ag.Name(), sessionID)
	var relPath string
	switch {
	case projectID != "" && chatID != "":
		relPath = "projects/" + projectID + "/" + chatID + "/todo.md"
	case chatID != "":
		relPath = "sessions/" + chatID + "/todo.md"
	default:
		jsonResponse(w, http.StatusOK, map[string]any{"items": []any{}, "raw": ""})
		return
	}

	raw, err := s.ws.readWorkspaceFileBytes(r.Context(), ag.Name(), relPath)
	if err != nil {
		// 404 / 未写入 / FS 未命中 — 返回空而不是显示错误；
		// 面板保持隐藏，直到 agent 写入一个。
		jsonResponse(w, http.StatusOK, map[string]any{"items": []any{}, "raw": ""})
		return
	}
	items := parseTodoMarkdown(string(raw))
	jsonResponse(w, http.StatusOK, map[string]any{
		"items": items,
		"raw":   string(raw),
	})
}

func parseTodoMarkdown(s string) []map[string]any {
	out := []map[string]any{}
	idx := map[string]int{}
	for _, line := range strings.Split(s, "\n") {
		trim := strings.TrimLeft(line, " \t")
		if !strings.HasPrefix(trim, "- [") && !strings.HasPrefix(trim, "* [") {
			continue
		}
		if len(trim) < 6 {
			continue
		}
		box := trim[3]
		rest := strings.TrimSpace(trim[5:])
		if rest == "" {
			continue
		}
		done := box == 'x' || box == 'X'
		if i, ok := idx[rest]; ok {
			if done {
				out[i]["done"] = true
			}
			continue
		}
		idx[rest] = len(out)
		out = append(out, map[string]any{
			"text": rest,
			"done": done,
		})
	}
	return out
}

func (s *ChatHandler) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	sessionID := r.URL.Query().Get("sessionId")
	ag := s.guard.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	resp := map[string]any{"history": ag.WebChatHistory(sessionID)}
	// latestEventSeq 是 /api/chat/subscribe 的恢复游标 —
	// 客户端用 `since=<latestEventSeq>` 打开该端点，
	// 以便新页面加载只拾取尚未渲染的增量。
	// 尽力而为：丢失/零值仅表示"仅实时流，无回放"，
	// 当会话没有进行中的轮次或 session_events 未被回填时，这是正确的回退。
	if s.dataStore != nil {
		uid := effectiveUserID(r)
		if uid != "" {
			if seq, err := s.dataStore.LatestSessionEventSeq(r.Context(), uid, ag.Name(), sessionID); err == nil {
				resp["latestEventSeq"] = seq
			}
		}
	}
	jsonResponse(w, http.StatusOK, resp)
}

func (s *ChatHandler) handleChatSessions(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	ag := s.guard.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusOK, map[string]any{"sessions": []session.WebSession{}})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"sessions": ag.WebChatSessions()})
}

func (s *ChatHandler) handleRenameSession(w http.ResponseWriter, r *http.Request) {
	// agentId 来自 body 或 query — 前端在 JSON body 中发送它
	//（参见 web/src/lib/api.ts 中的 renameChatSession），与 handleMoveSessionProject 约定一致。
	// 以前的仅 query 路径总是看到 "" 并在 resolveAgent 处以静默 404 退出，
	// 因此即使对话框正常提交，"Edit chat title"也看起来像无操作。
	agentID := r.URL.Query().Get("agentId")
	var req struct {
		AgentID string `json:"agentId"`
		Title   string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if agentID == "" {
		agentID = req.AgentID
	}
	ag := s.guard.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if err := ag.RenameWebChatSession(r.PathValue("key"), req.Title); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *ChatHandler) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	ag := s.guard.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if err := ag.DeleteWebChatSession(r.PathValue("key")); err != nil {
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}

// handleMoveSessionProject 将一个聊天重新分配给不同的项目
// （或在 projectId 为 "" 时将其分离回松散聊天列表）。
// 支持侧边栏拖放功能：将聊天行拖到项目标题上/从项目标题拖出会触发此端点。
//
// 请求体：{ "agentId": "...", "projectId": "<pid>" | "" }
//
// 除 sessions.project_id 翻转之外的副作用：
//   - workspace 文件在 sessions/<sid>/ 和 projects/<pid>/<sid>/ 之间移动，
//     以便下一个轮次在新作用域下看到自己的工件。空源目录 = 无操作。
//   - 绑定到此聊天的任何活跃 sandbox 被释放，以便替换容器以新的绑定挂载路径启动。
//
// 当目标目录已有文件时返回 409，code="destination_exists"
// （防御性 — session_keys 是唯一的，因此这不应自然发生，但比静默合并好）。
func (s *ChatHandler) handleMoveSessionProject(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agentId")
	var req struct {
		AgentID   string `json:"agentId"`
		ProjectID string `json:"projectId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if agentID == "" {
		agentID = req.AgentID
	}
	if agentID == "" {
		jsonResponse(w, http.StatusBadRequest, map[string]any{"error": "agentId required"})
		return
	}
	// 仅拥有者 — 移动聊天会更改其 workspace 路径，只读查看器绝不应触发。
	if rec := s.guard.requireAgentOwner(w, r, agentID); rec == nil {
		return
	}
	if !requireWritable(w, r) {
		return
	}
	uid := effectiveUserID(r)
	// 验证目标项目存在且属于此调用者。
	// 空的 projectId 是"分离"情况 — 始终允许。
	if req.ProjectID != "" && s.dataStore != nil {
		p, err := s.dataStore.GetProject(r.Context(), uid, agentID, req.ProjectID)
		if err != nil {
			jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if p == nil {
			jsonResponse(w, http.StatusNotFound, map[string]any{"error": "project not found"})
			return
		}
	}
	ag := s.guard.resolveAgent(r, agentID)
	if ag == nil {
		jsonResponse(w, http.StatusNotFound, map[string]any{"error": "agent not found"})
		return
	}
	if err := ag.MoveWebChatSession(r.Context(), r.PathValue("key"), req.ProjectID); err != nil {
		if errors.Is(err, workspace.ErrMoveDestinationExists) {
			jsonResponse(w, http.StatusConflict, map[string]any{
				"error": "destination workspace already exists",
				"code":  "destination_exists",
			})
			return
		}
		jsonResponse(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"ok": true})
}
