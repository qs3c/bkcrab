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

const (
	DeployStatusDeployed = "deployed"
	DeployStatusExisted  = "existed"
	DeployStatusReplaced = "replaced"
	DeployStatusFailed   = "failed"
)

type GatewayServiceDeployResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

type GatewayDeploymentSummary struct {
	Total    int `json:"total"`
	Existed  int `json:"existed"`
	Deployed int `json:"deployed"`
	Replaced int `json:"replaced"`
	Failed   int `json:"failed"`
}

type GatewayDeployResponse struct {
	Success bool                                  `json:"success"`
	Message string                                `json:"message"`
	Results map[string]GatewayServiceDeployResult `json:"results"`
	Summary GatewayDeploymentSummary              `json:"summary"`
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
	if dst.Command != "" {
		// The v2.1.0 gateway's aggregated /stream endpoint subscribes to
		// command-backed services through their SSE bridge. Marking a stdio
		// service as streamhttp leaves that bridge URL empty.
		dst.GatewayProtocol = "sse"
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

func DeployToGateway(ctx context.Context, client *http.Client, baseURL string, servers map[string]config.MCPServerConfig) (*GatewayDeployResponse, error) {
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
			return nil, fmt.Errorf("convert MCP server %q: %w", name, err)
		}
		converted[name] = cfg
	}
	body := struct {
		MCPServers map[string]LuckyServerConfig `json:"mcpServers"`
	}{MCPServers: converted}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal deploy payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/deploy", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("create deploy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("deploy to gateway: %w", err)
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("read gateway deploy response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gateway deploy HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result GatewayDeployResponse
	trimmed := bytes.TrimSpace(respBody)
	if len(trimmed) > 0 {
		if err := json.Unmarshal(trimmed, &result); err != nil {
			return nil, fmt.Errorf("decode gateway deploy HTTP %d response: %w", resp.StatusCode, err)
		}
	}

	// Older gateway builds returned an empty body or {"ok":true}. Preserve
	// compatibility for a normal 200 response by treating every requested
	// service as accepted. A 206 must always identify the failed services.
	if len(result.Results) == 0 {
		if resp.StatusCode == http.StatusPartialContent {
			return nil, fmt.Errorf("gateway deploy HTTP 206 did not include per-service results")
		}
		result.Success = true
		result.Message = "gateway accepted deployment"
		result.Results = make(map[string]GatewayServiceDeployResult, len(converted))
		for name := range converted {
			result.Results[name] = GatewayServiceDeployResult{
				Name:    name,
				Status:  DeployStatusDeployed,
				Message: "gateway accepted deployment",
			}
		}
	}

	normalizeGatewayDeployResponse(&result, converted)
	return &result, nil
}

func normalizeGatewayDeployResponse(result *GatewayDeployResponse, requested map[string]LuckyServerConfig) {
	if result.Results == nil {
		result.Results = map[string]GatewayServiceDeployResult{}
	}
	summary := GatewayDeploymentSummary{Total: len(requested)}
	for name := range requested {
		item, ok := result.Results[name]
		if !ok {
			item = GatewayServiceDeployResult{
				Name:    name,
				Status:  DeployStatusFailed,
				Message: "网关未返回该服务的部署结果",
				Error:   "missing per-service deployment result",
			}
		}
		item.Name = name
		item.Status = strings.ToLower(strings.TrimSpace(item.Status))
		switch item.Status {
		case DeployStatusDeployed:
			summary.Deployed++
		case DeployStatusExisted:
			summary.Existed++
		case DeployStatusReplaced:
			summary.Replaced++
		case DeployStatusFailed:
			summary.Failed++
		default:
			item.Status = DeployStatusFailed
			if item.Error == "" {
				item.Error = "unknown gateway deployment status"
			}
			if item.Message == "" {
				item.Message = "网关返回了未知的部署状态"
			}
			summary.Failed++
		}
		result.Results[name] = item
	}
	result.Summary = summary
	result.Success = summary.Failed == 0
	if result.Message == "" {
		if result.Success {
			result.Message = fmt.Sprintf("部署完成：%d 个服务成功", summary.Total)
		} else {
			result.Message = fmt.Sprintf("部署完成但有 %d/%d 个服务失败", summary.Failed, summary.Total)
		}
	}
}

func (r *GatewayDeployResponse) FailedResults(names []string) map[string]GatewayServiceDeployResult {
	out := map[string]GatewayServiceDeployResult{}
	if r == nil {
		return out
	}
	if len(names) == 0 {
		names = make([]string, 0, len(r.Results))
		for name := range r.Results {
			names = append(names, name)
		}
	}
	for _, name := range names {
		item, ok := r.Results[name]
		if !ok || item.Status == DeployStatusFailed {
			if !ok {
				item = GatewayServiceDeployResult{
					Name:    name,
					Status:  DeployStatusFailed,
					Message: "网关未返回该服务的部署结果",
				}
			}
			out[name] = item
		}
	}
	return out
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
