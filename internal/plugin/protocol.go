package plugin

import "encoding/json"

// JSON-RPC 2.0 类型，用于通过 stdin/stdout 的插件通信。

// Request 是发送给插件的 JSON-RPC 2.0 请求。
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      int             `json:"id"`
}

// Response 是来自插件的 JSON-RPC 2.0 响应。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      int             `json:"id"`
}

// Notification 是来自插件的 JSON-RPC 2.0 通知（无 ID）。
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError 是 JSON-RPC 2.0 错误对象。
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// 标准 JSON-RPC 方法。
const (
	MethodInitialize  = "initialize"
	MethodShutdown    = "shutdown"
	MethodChannelSend = "channel.send"
	MethodToolList    = "tool.list"
	MethodToolExecute = "tool.execute"
	// MethodProviderList 询问插件它填充了哪些工具提供者槽位
	// （例如 `{"category":"web_search","name":"kagi"}`）。未实现
	// 该方法的插件返回空列表或"method not found"。
	MethodProviderList = "provider.list"
	// MethodProviderExecute 调用插件内部的特定提供者。
	// 该调用由与路由内置提供者相同的 Chain 逻辑编排，
	// 因此插件与进程内提供者在平等基础上竞争（优先级、回退）。
	MethodProviderExecute = "provider.execute"
	MethodHookRegister    = "hook.register"
	MethodHookFire        = "hook.fire"
	MethodMessageInbound  = "message.inbound"
	// MethodChatSend: plugin → bkcrab 通知，将新的出站消息发送到
	// 指定的聊天。由钩子插件（post-turn TTS、翻译等）使用，用于向
	// 代理刚刚回复的同一聊天添加后续内容。与 message.inbound
	// （会触发另一个代理轮次）不同，此方法绕过代理直接进入
	// 总线出站路径。
	MethodChatSend = "chat.send"
)

// InitializeParams 随 initialize 方法发送。
type InitializeParams struct {
	Config map[string]interface{} `json:"config"`
}

// ChannelSendParams 随 channel.send 发送。
type ChannelSendParams struct {
	ChatID string `json:"chatId"`
	Text   string `json:"text"`
}

// ToolListResult 从 tool.list 返回。
type ToolListResult struct {
	Tools []ToolDef `json:"tools"`
}

// ToolDef 描述插件提供的工具。
type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

// ToolExecuteParams 随 tool.execute 发送。
type ToolExecuteParams struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

// ToolExecuteResult 从 tool.execute 返回。
type ToolExecuteResult struct {
	Result string `json:"result"`
}

// ProviderDef 描述插件可以填充的一个工具提供者槽位。插件
// 可以声明多个此类槽位（例如，一个插件同时暴露
// web_search 和 image_gen 提供者）。
type ProviderDef struct {
	Category string `json:"category"` // "web_search" / "image_gen" / ...
	Name     string `json:"name"`     // e.g. "kagi"
}

// ProviderListResult 从 provider.list 返回。
type ProviderListResult struct {
	Providers []ProviderDef `json:"providers"`
}

// ProviderExecuteParams 携带每次调用的参数和已解析的租户
// 配置（API 密钥、端点、额外选项、模型 ID）。插件进程
// 不得缓存凭据——BkCrab 在每次调用时重新发送它们，
// 以便任何租户都可以安全地使用同一插件进程。
type ProviderExecuteParams struct {
	Category string                 `json:"category"`
	Name     string                 `json:"name"`
	Args     map[string]interface{} `json:"args"`
	Config   ProviderConfigWire     `json:"config"`
}

// ProviderConfigWire 在 JSON-RPC 边界上镜像
// toolproviders.ProviderConfig。结构上保持独立，以使
// plugin 包不依赖 internal/toolproviders。
type ProviderConfigWire struct {
	APIKey   string            `json:"apiKey,omitempty"`
	Endpoint string            `json:"endpoint,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
	Model    string            `json:"model,omitempty"`
}

// ProviderExecuteResult 携带提供者的响应。Text 是
// LLM 可见的输出；Retriable 表示非空错误是否应
// 触发回退到下一个提供者。
type ProviderExecuteResult struct {
	Text      string `json:"text"`
	Error     string `json:"error,omitempty"`
	Retriable bool   `json:"retriable,omitempty"`
}

// InboundMessageParams 由通道插件通过 message.inbound 通知发送。
type InboundMessageParams struct {
	Channel    string `json:"channel"`
	ChatID     string `json:"chatId"`
	UserID     string `json:"userId"`
	Text       string `json:"text"`
	PeerKind   string `json:"peerKind,omitempty"`
	SenderName string `json:"senderName,omitempty"`
}

// HookRegisterResult 从 hook.register 返回。
type HookRegisterResult struct {
	Points []string `json:"points"`
}

// HookFireParams 随 hook.fire 发送。
//
// Channel / AccountID 为插件提供总线路由三元组，用于通过
// chat.send 将后续消息回显到同一聊天。这些字段是在
// chat.send 方法发布时添加的——仅读取 AgentName / ChatID / UserID
// 的旧插件仍然可以工作，因为新字段是附加的。
type HookFireParams struct {
	Point      string            `json:"point"`
	AgentName  string            `json:"agentName"`
	Channel    string            `json:"channel,omitempty"`
	AccountID  string            `json:"accountId,omitempty"`
	ChatID     string            `json:"chatId"`
	UserID     string            `json:"userId,omitempty"`
	Messages   []HookMessage     `json:"messages,omitempty"`
	Response   *HookResponseData `json:"response,omitempty"`
	ToolName   string            `json:"toolName,omitempty"`
	ToolArgs   string            `json:"toolArgs,omitempty"`
	ToolResult string            `json:"toolResult,omitempty"`
}

// ChatSendParams: plugin → bkcrab 将出站消息推送到
// 指定聊天。插件管理器根据这些字段构建 bus.OutboundMessage
// 并将其推送到 bus.Outbound——与通道适配器或代理循环
// 使用的路径相同。与 message.inbound（模拟新的用户入站
// 并触发代理轮次）不同：chat.send 直接发送给用户，
// 而不再次调用代理。
//
// 由 PostTurn 钩子插件使用，用于向代理刚刚回复的
// 同一聊天添加后续内容（音频、翻译、摘要等）。
// 插件会回显它在之前的 hook.fire 的 HookFireParams 中
// 接收到的 Channel / AccountID / ChatID。
type ChatSendParams struct {
	Channel   string          `json:"channel"`
	AccountID string          `json:"accountId,omitempty"`
	ChatID    string          `json:"chatId"`
	AgentID   string          `json:"agentId,omitempty"` // 用于 Web SSE 路由
	Text      string          `json:"text,omitempty"`
	Media     []ChatSendMedia `json:"media,omitempty"`
}

// ChatSendMedia 是 ChatSendParams 中的一个附件。BytesB64 是
// 文件的 base64 编码字节（以便 JSON-RPC 可以通过
// stdin/stdout 传输二进制数据）。ContentType 是可选的——
// 当为空时，通道适配器会从文件名/字节中嗅探。
type ChatSendMedia struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType,omitempty"`
	BytesB64    string `json:"bytesB64"`
}

// HookMessage 是用于钩子通信的简化消息。
type HookMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// HookResponseData 是用于钩子通信的简化响应。
type HookResponseData struct {
	Content  string `json:"content"`
	HasTools bool   `json:"hasTools"`
}

// HookFireResult 从 hook.fire 返回（用于同步钩子）。
type HookFireResult struct {
	Messages []HookMessage `json:"messages,omitempty"`
}

// newRequest 创建一个 JSON-RPC 2.0 请求。
func newRequest(method string, params interface{}, id int) (*Request, error) {
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return &Request{
		JSONRPC: "2.0",
		Method:  method,
		Params:  raw,
		ID:      id,
	}, nil
}
