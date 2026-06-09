package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// StdioClient implements the MCP client for stdio-based servers.
type StdioClient struct {
	command string
	args    []string
	env     map[string]string
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	scanner *bufio.Scanner
	mu      sync.Mutex
	nextID  int
}

// NewStdioClient creates a new stdio MCP client.
func NewStdioClient(command string, args []string, env map[string]string) *StdioClient {
	return &StdioClient{
		command: command,
		args:    args,
		env:     env,
		nextID:  1,
	}
}

// Connect starts the subprocess and initializes the MCP session.
func (c *StdioClient) Connect() error {
	c.cmd = exec.Command(c.command, c.args...)

	// Set environment variables
	c.cmd.Env = os.Environ()
	for k, v := range c.env {
		if strings.HasPrefix(v, "$") {
			v = os.Getenv(v[1:])
		}
		c.cmd.Env = append(c.cmd.Env, k+"="+v)
	}

	var err error
	c.stdin, err = c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}

	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	c.cmd.Stderr = os.Stderr
	c.scanner = bufio.NewScanner(stdout)
	c.scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer

	if err := c.cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}

	// Send initialize
	_, err = c.sendRequest("initialize", initializeParams{
		ProtocolVersion: "2024-11-05",
		ClientInfo:      clientInfo{Name: "fastclaw", Version: "0.1.0"},
	})
	return err
}

func (c *StdioClient) sendRequest(method string, params interface{}) (*jsonRPCResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	id := c.nextID
	c.nextID++

	req := jsonRPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	data = append(data, '\n')
	if _, err := c.stdin.Write(data); err != nil {
		return nil, fmt.Errorf("write to stdin: %w", err)
	}

	// Read response lines until we get a JSON-RPC response with matching ID
	for c.scanner.Scan() {
		line := c.scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var resp jsonRPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			continue // skip non-JSON lines (e.g. stderr leaking)
		}

		if resp.ID == id {
			if resp.Error != nil {
				return nil, fmt.Errorf("RPC error %d: %s", resp.Error.Code, resp.Error.Message)
			}
			return &resp, nil
		}
	}

	if err := c.scanner.Err(); err != nil {
		return nil, fmt.Errorf("read stdout: %w", err)
	}
	return nil, fmt.Errorf("process exited without response")
}

// ListTools returns the list of tools available on the MCP server.
func (c *StdioClient) ListTools() ([]ToolDef, error) {
	resp, err := c.sendRequest("tools/list", struct{}{})
	if err != nil {
		return nil, err
	}

	var result toolsListResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return nil, fmt.Errorf("parse tools list: %w", err)
	}

	return result.Tools, nil
}

// CallTool calls a tool on the MCP server.
func (c *StdioClient) CallTool(name string, args json.RawMessage) (string, error) {
	resp, err := c.sendRequest("tools/call", toolCallParams{
		Name:      name,
		Arguments: args,
	})
	if err != nil {
		return "", err
	}

	var result toolCallResult
	if err := json.Unmarshal(resp.Result, &result); err != nil {
		return "", fmt.Errorf("parse tool result: %w", err)
	}

	var texts []string
	for _, ct := range result.Content {
		if ct.Type == "text" {
			texts = append(texts, ct.Text)
		}
	}
	return strings.Join(texts, "\n"), nil
}

// Close stops the subprocess.
func (c *StdioClient) Close() error {
	if c.stdin != nil {
		c.stdin.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		c.cmd.Process.Kill()
		c.cmd.Wait()
	}
	return nil
}
