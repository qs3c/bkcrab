package mcp

import (
	"encoding/json"
	"fmt"

	"github.com/qs3c/bkcrab/internal/config"
)

type Client interface {
	Connect() error
	ListTools() ([]ToolDef, error)
	CallTool(name string, args json.RawMessage) (string, error)
	Close() error
}

type ClientFactory func(name string, cfg config.MCPServerConfig) (Client, error)

func NewClientFromConfig(name string, cfg config.MCPServerConfig) (Client, error) {
	switch cfg.Type {
	case "http":
		switch config.NormalizeMCPTransport(cfg.Transport) {
		case config.MCPTransportStreamableHTTP:
			return NewStreamableHTTPClient(cfg.URL, cfg.Headers), nil
		case config.MCPTransportSSE:
			return NewGatewaySSEClient(cfg.URL, cfg.Headers), nil
		default:
			return NewHTTPClient(cfg.URL, cfg.Headers), nil
		}
	case "stdio":
		return NewStdioClient(cfg.Command, cfg.Args, cfg.Env), nil
	default:
		return nil, fmt.Errorf("unknown MCP server type %q for %s", cfg.Type, name)
	}
}

type ToolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema interface{} `json:"inputSchema"`
}

type jsonRPCRequest struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      int         `json:"id,omitempty"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params,omitempty"`
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
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    struct{}   `json:"capabilities"`
	ClientInfo      clientInfo `json:"clientInfo"`
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
