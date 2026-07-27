package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
)

type LuckyServerConfig struct {
	URL             string            `json:"url,omitempty"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	GatewayProtocol string            `json:"gateway_protocol,omitempty"`
}

func ToLuckyServerConfig(src config.MCPServerConfig) (LuckyServerConfig, error) {
	dst := LuckyServerConfig{
		URL:             strings.TrimSpace(src.URL),
		Command:         strings.TrimSpace(src.Command),
		Args:            append([]string(nil), src.Args...),
		Env:             copyStringMap(src.Env),
		GatewayProtocol: luckyProtocol(src.Transport),
	}
	if dst.Env == nil {
		dst.Env = map[string]string{}
	}
	if auth := strings.TrimSpace(src.Headers["Authorization"]); auth != "" {
		const prefix = "Bearer "
		if !strings.HasPrefix(auth, prefix) {
			return dst, fmt.Errorf("only Authorization: Bearer tokens are supported for remote HTTP MCP servers")
		}
		dst.Env["MCP_REMOTE_AUTH_ACCESS_TOKEN"] = strings.TrimSpace(strings.TrimPrefix(auth, prefix))
	}
	for k, v := range src.Headers {
		if strings.EqualFold(k, "Authorization") && strings.TrimSpace(v) != "" {
			continue
		}
		if strings.TrimSpace(v) != "" {
			return dst, fmt.Errorf("header %q is not supported by the selected MCP gateway", k)
		}
	}
	return dst, nil
}

func DeployToGateway(ctx context.Context, client *http.Client, baseURL string, servers map[string]config.MCPServerConfig) error {
	if client == nil {
		client = http.DefaultClient
	}
	converted := make(map[string]LuckyServerConfig, len(servers))
	for name, server := range servers {
		if !config.MCPServerEnabled(server) {
			continue
		}
		cfg, err := ToLuckyServerConfig(server)
		if err != nil {
			return fmt.Errorf("convert MCP server %q: %w", name, err)
		}
		converted[name] = cfg
	}
	body := struct {
		MCPServers map[string]LuckyServerConfig `json:"mcpServers"`
	}{MCPServers: converted}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal deploy payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/deploy", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create deploy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("deploy to gateway: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gateway deploy HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func luckyProtocol(v string) string {
	switch config.NormalizeMCPTransport(v) {
	case config.MCPTransportSSE:
		return "sse"
	default:
		return "streamhttp"
	}
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
