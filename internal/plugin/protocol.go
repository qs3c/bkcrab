package plugin

import "encoding/json"

// JSON-RPC 2.0 types for plugin communication over stdin/stdout.

// Request is a JSON-RPC 2.0 request sent to a plugin.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      int             `json:"id"`
}

// Response is a JSON-RPC 2.0 response from a plugin.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
	ID      int             `json:"id"`
}

// Notification is a JSON-RPC 2.0 notification (no ID) from a plugin.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// RPCError is a JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Message }

// Standard JSON-RPC methods.
const (
	MethodInitialize     = "initialize"
	MethodShutdown       = "shutdown"
	MethodChannelSend    = "channel.send"
	MethodToolList       = "tool.list"
	MethodToolExecute    = "tool.execute"
	// MethodProviderList asks a plugin which tool-provider slots it fills
	// (e.g. `{"category":"web_search","name":"kagi"}`). Plugins that don't
	// implement it return an empty list or "method not found".
	MethodProviderList = "provider.list"
	// MethodProviderExecute invokes a specific provider inside the plugin.
	// The call is orchestrated by the same Chain logic that routes built-in
	// providers, so plugins compete with in-process providers on an equal
	// footing (priority, fallback).
	MethodProviderExecute = "provider.execute"
	MethodHookRegister   = "hook.register"
	MethodHookFire       = "hook.fire"
	MethodMessageInbound = "message.inbound"
	// MethodChatSend: plugin → fastclaw notification that delivers a
	// new outbound message to a specific chat. Used by hook plugins
	// (post-turn TTS, translation, etc.) to add follow-up content to
	// the same chat the agent just replied to. Unlike message.inbound
	// (which spawns another agent turn), this skips the agent and goes
	// straight to the bus outbound path.
	MethodChatSend = "chat.send"
)

// InitializeParams is sent with the initialize method.
type InitializeParams struct {
	Config map[string]interface{} `json:"config"`
}

// ChannelSendParams is sent with channel.send.
type ChannelSendParams struct {
	ChatID string `json:"chatId"`
	Text   string `json:"text"`
}

// ToolListResult is returned from tool.list.
type ToolListResult struct {
	Tools []ToolDef `json:"tools"`
}

// ToolDef describes a tool provided by a plugin.
type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Parameters  interface{} `json:"parameters,omitempty"`
}

// ToolExecuteParams is sent with tool.execute.
type ToolExecuteParams struct {
	Name string                 `json:"name"`
	Args map[string]interface{} `json:"args"`
}

// ToolExecuteResult is returned from tool.execute.
type ToolExecuteResult struct {
	Result string `json:"result"`
}

// ProviderDef describes one tool-provider slot a plugin can fill. Plugins
// can advertise multiple of these (e.g. one plugin exposing both a
// web_search and an image_gen provider).
type ProviderDef struct {
	Category string `json:"category"` // "web_search" / "image_gen" / ...
	Name     string `json:"name"`     // e.g. "kagi"
}

// ProviderListResult is returned from provider.list.
type ProviderListResult struct {
	Providers []ProviderDef `json:"providers"`
}

// ProviderExecuteParams carries the per-call args and the resolved tenant
// config (API key, endpoint, extra options, model id). The plugin process
// must not cache credentials — FastClaw re-sends them every call so any
// tenant can use the same plugin process safely.
type ProviderExecuteParams struct {
	Category string                 `json:"category"`
	Name     string                 `json:"name"`
	Args     map[string]interface{} `json:"args"`
	Config   ProviderConfigWire     `json:"config"`
}

// ProviderConfigWire mirrors toolproviders.ProviderConfig over the JSON-RPC
// boundary. Structurally distinct to keep the plugin package free of a
// dependency on internal/toolproviders.
type ProviderConfigWire struct {
	APIKey   string            `json:"apiKey,omitempty"`
	Endpoint string            `json:"endpoint,omitempty"`
	Options  map[string]string `json:"options,omitempty"`
	Model    string            `json:"model,omitempty"`
}

// ProviderExecuteResult carries the provider's response. Text is the
// LLM-visible output; Retriable signals whether a non-empty error should
// trigger fallback to the next provider.
type ProviderExecuteResult struct {
	Text      string `json:"text"`
	Error     string `json:"error,omitempty"`
	Retriable bool   `json:"retriable,omitempty"`
}

// InboundMessageParams is sent by channel plugins via message.inbound notifications.
type InboundMessageParams struct {
	Channel    string `json:"channel"`
	ChatID     string `json:"chatId"`
	UserID     string `json:"userId"`
	Text       string `json:"text"`
	PeerKind   string `json:"peerKind,omitempty"`
	SenderName string `json:"senderName,omitempty"`
}

// HookRegisterResult is returned from hook.register.
type HookRegisterResult struct {
	Points []string `json:"points"`
}

// HookFireParams is sent with hook.fire.
//
// Channel / AccountID give the plugin the bus routing triple needed to
// echo a follow-up message back to the same chat via chat.send. They
// were added when the chat.send method shipped — older plugins that
// only read AgentName / ChatID / UserID keep working since the new
// fields are additive.
type HookFireParams struct {
	Point      string             `json:"point"`
	AgentName  string             `json:"agentName"`
	Channel    string             `json:"channel,omitempty"`
	AccountID  string             `json:"accountId,omitempty"`
	ChatID     string             `json:"chatId"`
	UserID     string             `json:"userId,omitempty"`
	Messages   []HookMessage      `json:"messages,omitempty"`
	Response   *HookResponseData  `json:"response,omitempty"`
	ToolName   string             `json:"toolName,omitempty"`
	ToolArgs   string             `json:"toolArgs,omitempty"`
	ToolResult string             `json:"toolResult,omitempty"`
}

// ChatSendParams: plugin → fastclaw push of an outbound message to a
// specific chat. The plugin manager constructs a bus.OutboundMessage
// from these fields and pushes it onto bus.Outbound — same path a
// channel adapter or the agent loop would use. Distinct from
// message.inbound (which simulates a new user inbound and triggers
// an agent turn): chat.send delivers TO the user without invoking
// the agent again.
//
// Used by PostTurn hook plugins to add follow-up content (audio,
// translations, summaries, etc.) to the same chat the agent just
// replied to. Plugin echoes the Channel / AccountID / ChatID it
// received in the prior hook.fire's HookFireParams.
type ChatSendParams struct {
	Channel   string          `json:"channel"`
	AccountID string          `json:"accountId,omitempty"`
	ChatID    string          `json:"chatId"`
	AgentID   string          `json:"agentId,omitempty"` // used by web SSE routing
	Text      string          `json:"text,omitempty"`
	Media     []ChatSendMedia `json:"media,omitempty"`
}

// ChatSendMedia is one attachment in a ChatSendParams. BytesB64 is the
// file's bytes base64-encoded (so JSON-RPC can ship binary over
// stdin/stdout). ContentType is optional — the channel adapter will
// sniff from the filename / bytes when empty.
type ChatSendMedia struct {
	Filename    string `json:"filename"`
	ContentType string `json:"contentType,omitempty"`
	BytesB64    string `json:"bytesB64"`
}

// HookMessage is a simplified message for hook communication.
type HookMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// HookResponseData is a simplified response for hook communication.
type HookResponseData struct {
	Content   string `json:"content"`
	HasTools  bool   `json:"hasTools"`
}

// HookFireResult is returned from hook.fire (for synchronous hooks).
type HookFireResult struct {
	Messages []HookMessage `json:"messages,omitempty"`
}

// newRequest creates a JSON-RPC 2.0 request.
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
