package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/qs3c/bkcrab/internal/config"
)

type Manager struct {
	servers    map[string]Client
	toolMap    map[string]toolRoute
	toolDefs   map[string]ToolDef
	closeHooks []func()
}

type toolRoute struct {
	serverName   string
	originalName string
}

func NewManager(servers map[string]config.MCPServerConfig) *Manager {
	return NewManagerWithFactory(servers, NewClientFromConfig)
}

func NewManagerWithFactory(servers map[string]config.MCPServerConfig, factory ClientFactory) *Manager {
	m := newEmptyManager()

	for name, cfg := range servers {
		if !config.MCPServerEnabled(cfg) {
			continue
		}
		client, err := factory(name, cfg)
		if err != nil {
			slog.Warn("failed to create MCP client, skipping", "server", name, "error", err)
			continue
		}
		if err := client.Connect(); err != nil {
			slog.Warn("failed to connect to MCP server, skipping", "server", name, "error", err)
			continue
		}
		tools, err := client.ListTools()
		if err != nil {
			slog.Warn("failed to list MCP tools, skipping", "server", name, "error", err)
			_ = client.Close()
			continue
		}

		m.servers[name] = client
		for _, t := range tools {
			prefixed := prefixToolName(name, t.Name)
			m.toolMap[prefixed] = toolRoute{serverName: name, originalName: t.Name}
			m.toolDefs[prefixed] = ToolDef{
				Name:        prefixed,
				Description: t.Description,
				InputSchema: t.InputSchema,
			}
		}
		slog.Info("connected to MCP server", "server", name, "tools", len(tools))
	}

	return m
}

func NewAggregatedManager(client Client) *Manager {
	m := newEmptyManager()
	m.servers["gateway"] = client

	if err := client.Connect(); err != nil {
		slog.Warn("failed to connect to aggregated MCP gateway", "error", err)
		return m
	}
	tools, err := client.ListTools()
	if err != nil {
		slog.Warn("failed to list aggregated MCP gateway tools", "error", err)
		_ = client.Close()
		return m
	}
	for _, t := range tools {
		prefixed := prefixAggregatedToolName(t.Name)
		m.toolMap[prefixed] = toolRoute{
			serverName:   "gateway",
			originalName: t.Name,
		}
		m.toolDefs[prefixed] = ToolDef{
			Name:        prefixed,
			Description: t.Description,
			InputSchema: t.InputSchema,
		}
	}
	return m
}

func newEmptyManager() *Manager {
	return &Manager{
		servers:  make(map[string]Client),
		toolMap:  make(map[string]toolRoute),
		toolDefs: make(map[string]ToolDef),
	}
}

func (m *Manager) AddCloseHook(fn func()) {
	if fn != nil {
		m.closeHooks = append(m.closeHooks, fn)
	}
}

func (m *Manager) ToolDefs() []ToolDef {
	if m == nil {
		return nil
	}
	defs := make([]ToolDef, 0, len(m.toolDefs))
	for _, def := range m.toolDefs {
		defs = append(defs, def)
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

func (m *Manager) CallTool(_ context.Context, prefixedName string, args json.RawMessage) (string, error) {
	route, ok := m.toolMap[prefixedName]
	if !ok {
		return "", fmt.Errorf("unknown MCP tool: %s", prefixedName)
	}

	client, ok := m.servers[route.serverName]
	if !ok {
		return "", fmt.Errorf("MCP server not connected: %s", route.serverName)
	}

	return client.CallTool(route.originalName, args)
}

func (m *Manager) HasTools() bool {
	return m != nil && len(m.toolMap) > 0
}

func (m *Manager) Close() {
	if m == nil {
		return
	}
	for name, client := range m.servers {
		if err := client.Close(); err != nil {
			slog.Warn("error closing MCP server", "server", name, "error", err)
		}
	}
	for _, fn := range m.closeHooks {
		fn()
	}
	m.closeHooks = nil
}

func prefixToolName(serverName, toolName string) string {
	return "mcp_" + sanitizeToolName(serverName) + "_" + toolName
}

func prefixAggregatedToolName(toolName string) string {
	return "mcp_" + sanitizeToolName(toolName)
}

func sanitizeToolName(v string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '_'
	}, v)
}
