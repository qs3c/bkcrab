package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

const mcpSessionHeader = "Mcp-Session-Id"

type StreamableHTTPClient struct {
	url       string
	headers   map[string]string
	client    *http.Client
	mu        sync.Mutex
	nextID    int
	sessionID string
}

type streamableRPCResponse struct {
	*jsonRPCResponse
	SessionID string
}

func NewStreamableHTTPClient(url string, headers map[string]string) *StreamableHTTPClient {
	return &StreamableHTTPClient{
		url:     url,
		headers: expandHeaders(headers),
		client:  &http.Client{},
		nextID:  1,
	}
}

func (c *StreamableHTTPClient) Connect() error {
	resp, err := c.sendRequest("initialize", initializeParams{
		ProtocolVersion: "2025-03-26",
		ClientInfo:      clientInfo{Name: "bkcrab", Version: "0.1.0"},
	}, false)
	if err != nil {
		return err
	}
	if resp.SessionID == "" {
		return fmt.Errorf("streamable HTTP initialize did not return %s", mcpSessionHeader)
	}
	c.sessionID = resp.SessionID
	return c.sendNotification("notifications/initialized", struct{}{})
}

func (c *StreamableHTTPClient) ListTools() ([]ToolDef, error) {
	resp, err := c.sendRequest("tools/list", struct{}{}, true)
	if err != nil {
		return nil, err
	}

	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse tools list: %w", err)
	}

	return result.Tools, nil
}

func (c *StreamableHTTPClient) CallTool(name string, args json.RawMessage) (string, error) {
	resp, err := c.sendRequest("tools/call", toolCallParams{
		Name:      name,
		Arguments: args,
	}, true)
	if err != nil {
		return "", err
	}

	var result toolCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("parse tool result: %w", err)
	}

	var texts []string
	for _, content := range result.Content {
		if content.Type == "text" {
			texts = append(texts, content.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

func (c *StreamableHTTPClient) Close() error {
	if c.sessionID == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodDelete, c.url, nil)
	if err != nil {
		return fmt.Errorf("create close request: %w", err)
	}
	c.applyHeaders(req, true)

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("send close request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	c.sessionID = ""
	return nil
}

func (c *StreamableHTTPClient) sendRequest(method string, params interface{}, includeSession bool) (*streamableRPCResponse, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.mu.Unlock()

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	httpReq, err := c.newJSONRequest(req, includeSession)
	if err != nil {
		return nil, err
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	rpcResp, err := parseJSONRPCResponse(body)
	if err != nil {
		return nil, err
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return &streamableRPCResponse{
		jsonRPCResponse: rpcResp,
		SessionID:       resp.Header.Get(mcpSessionHeader),
	}, nil
}

func (c *StreamableHTTPClient) sendNotification(method string, params interface{}) error {
	req := jsonRPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	httpReq, err := c.newJSONRequest(req, true)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *StreamableHTTPClient) newJSONRequest(req jsonRPCRequest, includeSession bool) (*http.Request, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	c.applyHeaders(httpReq, includeSession)
	return httpReq, nil
}

func (c *StreamableHTTPClient) applyHeaders(req *http.Request, includeSession bool) {
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if includeSession && c.sessionID != "" {
		req.Header.Set(mcpSessionHeader, c.sessionID)
	}
}

func parseJSONRPCResponse(body []byte) (*jsonRPCResponse, error) {
	data := bytes.TrimSpace(body)
	if bytes.HasPrefix(data, []byte("data:")) || bytes.Contains(data, []byte("\ndata:")) {
		data = extractSSEData(data)
	}
	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(data, &rpcResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &rpcResp, nil
}

func extractSSEData(body []byte) []byte {
	var chunks [][]byte
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		chunks = append(chunks, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
	}
	if len(chunks) == 0 {
		return body
	}
	return bytes.Join(chunks, nil)
}
