package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// newLLMHTTPClient 返回一个适用于流式 LLM API 调用的 *http.Client。
//
// 为什么不用零值 &http.Client{}：它没有客户端级别的超时
//（这是我们想要的——长 SSE 流需要数分钟才能排空）但也没有
// ResponseHeaderTimeout，因此如果上游服务器在完成 TCP/TLS 握手之后、
// 但在返回头部之前停止写入，`client.Do` 将永远阻塞。
// 我们在生产环境中遇到过这种情况：对话中途静默，
// 每个聊天的 taskQueue 在被阻塞的请求后面排队，
// 每个后续的用户消息也排队，只有重启 Pod 才能恢复。
//
// DefaultTransport 已经提供了合理的 DialContext/Timeout（30 秒）、
// TLSHandshakeTimeout（10 秒）和 IdleConnTimeout（90 秒）——
// 我们只需要添加 ResponseHeaderTimeout。先调用 Clone()
// 是因为修改 DefaultTransport 会影响进程中的其他所有消费者
//（它是一个有文档记录的全局共享资源）。
//
// 60 秒对于 LLM API 来说是保守的：在健康路径上响应头部远在
// 1 秒内返回；60 秒可以在不误报的情况下捕获挂起的连接。
func newLLMHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{Transport: tr}
}

// Origin 标记由运行时而非真实用户/模型交互生成的消息。
// 空值表示"来自用户（或模型对用户的回应）"——常见情况。
// 过滤用户可见历史或跳过运行时插入内容的 FTS 索引的钩子以此为依据。
const (
	OriginUser        = "" // 默认值——现有生产者无需修改即可保持正确
	OriginGoalContext = "goal_context"
)

// Message 表示一条聊天消息。
// 存储在会话中时，保持所有字段与 LLM 返回的完全一致，
// 以确保后续轮次中提示缓存命中。
type Message struct {
	Role         string        `json:"role"`
	Content      string        `json:"content,omitempty"`
	ContentParts []ContentPart `json:"content_parts,omitempty"` // 多模态输入（用户消息）
	ToolCalls    []ToolCall    `json:"tool_calls,omitempty"`
	ToolCallID   string        `json:"tool_call_id,omitempty"`
	Name         string        `json:"name,omitempty"`
	Thinking     string        `json:"thinking,omitempty"`  // 模型的推理内容（用于记忆提取）
	Timestamp    int64         `json:"timestamp,omitempty"` // Unix 毫秒时间戳，用于记忆时间线

	// Metadata 是附加到工具角色消息上的 UI 状态（例如 { "sandbox": true }，
	// 以便聊天 UI 可以标记它）。不会发送给 LLM——
	// provider.toLLMMessages / anthropic / openai 序列化器会忽略它。
	Metadata map[string]any `json:"metadata,omitempty"`

	// RawAssistant 保留 API 返回的精确助手消息 JSON。
	// 将历史记录发送回 LLM 时，使用此字段而非从解析后的字段重新序列化——
	// 通过保持字节相同的前缀来保证提示缓存命中。
	// 仅在 role="assistant" 的消息上设置。
	RawAssistant json.RawMessage `json:"_raw,omitempty"`

	// Origin 区分运行时注入的消息与真实用户/助手的交互。
	// 空值（OriginUser）是默认值。目前仅 /goal 延续路径设置
	// OriginGoalContext。用户可见历史（WebChatHistory）和 FTS 索引
	// 过滤此字段，以便合成提示不会污染任一视图。
	// 作为 JSONB sessions.messages 工作集的一部分以及结构化
	// session_messages 归档中的一列存在。
	Origin string `json:"origin,omitempty"`
}

// TextContent 返回消息的用户可见文本。当 Content 为空时，
// 回退到拼接 ContentParts 中的 `text` 部分——
// 这是我们为带有附件的用户消息存储的格式
//（Content="" + ContentParts=[text, image_url, ...]）。
// 没有这个回退，基于 `Content != ""` 判断的历史/预览/标题代码
// 会静默丢弃每个多模态轮次。
//
// 同时还移除旧的客户端版本在发出的聊天文本前添加的
// "[Attached: <filename>]\n" 遗留面包屑痕迹。新版本不再添加它，
// 但历史会话中存储的内容仍然包含它；如果不在此处移除，
// 前缀会出现在聊天气泡、页面标题和侧边栏条目中。
func (m Message) TextContent() string {
	if m.Content != "" {
		return StripAttachedPrefix(m.Content)
	}
	var parts []string
	for _, p := range m.ContentParts {
		if p.Type == "text" && p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return StripAttachedPrefix(strings.Join(parts, "\n"))
}

// StripAttachedPrefix 从字符串中移除一个或多个前导的 "[Attached: …]" 标签
//（后跟可选的空白/换行）。公开此函数以便非 Message 的调用者
//（例如会话适配器中的原始存储行）可以应用相同的清理逻辑。
func StripAttachedPrefix(s string) string {
	for {
		if !strings.HasPrefix(s, "[Attached:") {
			break
		}
		end := strings.IndexByte(s, ']')
		if end < 0 {
			break
		}
		s = strings.TrimLeft(s[end+1:], " \t\r\n")
	}
	return s
}

// ContentPart 表示多模态内容的一部分。
type ContentPart struct {
	Type     string    `json:"type"` // "text" 或 "image_url"
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

// ImageURL 表示视觉消息的图片 URL。
type ImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // "auto"、"low"、"high"
}

// ToolCall 表示模型请求的函数调用。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 包含函数名称和参数。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool 描述一个可供模型使用的工具。
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction 描述一个函数工具。
type ToolFunction struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters"`
}

// Usage 报告提供者返回的令牌计数。零值是可以接受的——
// 管理员计量和目标预算将缺失的字段视为"未报告"；
// 严格需要计数的下游消费者（目标预算强制执行）
// 会检查零值 Usage 并回退到无限制行为。
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int // Anthropic 提示缓存命中令牌数（读取）
	CacheCreationTokens int // Anthropic 提示缓存写入令牌数
}

// Response 是 Chat 调用的结果。
type Response struct {
	Content      string
	ToolCalls    []ToolCall
	Thinking     string          // 模型的推理/思考内容（为记忆提取）
	Usage        Usage           // 用于计量的令牌计数（提供者未报告时为零）
	RawAssistant json.RawMessage // 精确的 API 响应消息 JSON（用于缓存安全的重放）
}

// HasToolCalls 如果响应包含工具调用则返回 true。
func (r *Response) HasToolCalls() bool {
	return len(r.ToolCalls) > 0
}

// StreamChunk 表示流式响应中的单个数据块。
type StreamChunk struct {
	Content   string
	ToolCalls []ToolCall
	Done      bool
	// Thinking 在 message_stop 时发送一次（如果模型产生了任何思考内容），
	// 以便调用者可以将其与最终助手消息一起持久化——
	// 这是必需的，以便下一轮可以将 content[].thinking 回显给
	// 支持扩展思考的提供者（Anthropic + DeepSeek 的 /anthropic 兼容模式）。
	Thinking          string
	ThinkingSignature string
	// Usage 在最终（Done）数据块上报告，如果提供者发出了令牌计数
	//（Anthropic message_delta.usage、OpenAI usage 块）。
	// 在中间数据块上为零。
	Usage Usage
	// RawAssistant 是提供者线路格式中完全序列化的助手消息，
	// 在最终（Done）数据块中发出。设置后，调用者应将其原样
	// 持久化到 Message.RawAssistant 而不是重新构建——
	// DeepSeek（OpenAI 兼容思考模式）在重放时需要看到正确的
	// 顶层 `reasoning_content`，而它不会自动生成。
	RawAssistant json.RawMessage
}

// StreamReader 从 LLM 响应中读取流式数据块。
type StreamReader struct {
	ch  chan StreamChunk
	err error
}

// NewStreamReader 使用给定的通道创建一个新的 StreamReader。
func NewStreamReader(ch chan StreamChunk) *StreamReader {
	return &StreamReader{ch: ch}
}

// Next 返回下一个数据块以及是否还有更多数据块可用。
func (r *StreamReader) Next() (StreamChunk, bool) {
	chunk, ok := <-r.ch
	return chunk, ok
}

// Err 返回流式处理期间发生的任何错误。
func (r *StreamReader) Err() error {
	return r.err
}

// SetErr 设置流读取器的错误。
func (r *StreamReader) SetErr(err error) {
	r.err = err
}

// Provider 是 LLM 提供者接口。
type Provider interface {
	Chat(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*Response, error)
	ChatStream(ctx context.Context, messages []Message, tools []Tool, model string, maxTokens int, temperature float64) (*StreamReader, error)
}

// StripProviderPrefix 从模型字符串中移除 "provider/" 前缀。
// 例如 "minimax-coding-plan/MiniMax-M2.7" -> "MiniMax-M2.7"
func StripProviderPrefix(model string) string {
	if idx := strings.Index(model, "/"); idx >= 0 {
		return model[idx+1:]
	}
	return model
}

// SplitProviderModel 将 "<providerKey>/<modelId>" 拆分为两部分。
// 当没有斜杠时（模型使用共享提供者，没有按代理覆盖），
// 第一个返回值为 ""，模型名称原样返回。"prov + / + model" 的逆操作。
func SplitProviderModel(s string) (provider, model string) {
	if idx := strings.Index(s, "/"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return "", s
}

// NewProvider 根据 apiType 创建 Provider。
// "anthropic-messages" 创建 Anthropic 提供者，其他情况创建 OpenAI 兼容的提供者。
func NewProvider(apiKey, apiBase, apiType string) Provider {
	if apiType == "anthropic-messages" {
		return NewAnthropic(apiKey, apiBase)
	}
	return NewOpenAI(apiKey, apiBase)
}
