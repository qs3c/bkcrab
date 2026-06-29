package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/qs3c/bkcrab/internal/agent/tools"
)

// RegisterPluginTools 查询工具插件获取其工具列表，并在给定的工具注册表中
// 注册它们。如果插件工具与内置工具同名，则插件版本会覆盖内置工具。
// 否则，工具会以限定名称注册（例如 "echo.echo_tool"）。
func RegisterPluginTools(ctx context.Context, mgr *Manager, pluginID string, registry *tools.Registry) error {
	toolDefs, err := mgr.ListTools(ctx, pluginID)
	if err != nil {
		return fmt.Errorf("list tools from plugin %s: %w", pluginID, err)
	}

	for _, td := range toolDefs {
		desc := td.Description
		params := td.Parameters
		toolName := td.Name

		fn := func(ctx context.Context, args json.RawMessage) (string, error) {
			var argsMap map[string]interface{}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &argsMap); err != nil {
					return "", fmt.Errorf("parse tool args: %w", err)
				}
			}
			if argsMap == nil {
				argsMap = make(map[string]interface{})
			}
			return mgr.ExecuteTool(ctx, pluginID, toolName, argsMap)
		}

		// 如果插件提供了与内置工具同名的工具，
		// 则用插件版本覆盖内置工具。
		if registry.HasBuiltin(toolName) {
			registry.RegisterFrom(toolName, desc, params, fn, tools.SourcePlugin)
			slog.Info("plugin: overriding built-in tool", "plugin", pluginID, "tool", toolName)
		} else {
			qualifiedName := pluginID + "." + toolName
			registry.RegisterFrom(qualifiedName, desc, params, fn, tools.SourcePlugin)
			slog.Info("plugin: registered tool", "plugin", pluginID, "tool", qualifiedName)
		}
	}

	return nil
}
