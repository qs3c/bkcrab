package mcp

import "encoding/json"

// Client 是与 MCP 服务器通信的接口。
type Client interface {
	Connect() error
	ListTools() ([]ToolDef, error)
	CallTool(name string, args json.RawMessage) (string, error)
	Close() error
}

// ToolDef 表示 MCP 服务器返回的工具定义。
type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

// JSON-RPC 2.0 类型

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type initializeParams struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    struct{}     `json:"capabilities"`
	ClientInfo      clientInfo   `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type toolsListResult struct {
	Tools []ToolDef `json:"tools"`
}

type toolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type toolCallResult struct {
	Content []toolContent `json:"content"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
