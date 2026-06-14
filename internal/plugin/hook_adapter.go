package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/qs3c/bkclaw/internal/agent"
	"github.com/qs3c/bkclaw/internal/provider"
)

// hookPointName 将 HookPoint 常量映射为蛇形命名法的协议名称。
var hookPointName = map[agent.HookPoint]string{
	agent.BeforeModelCall: "before_model_call",
	agent.AfterModelCall:  "after_model_call",
	agent.BeforeToolCall:  "before_tool_call",
	agent.AfterToolCall:   "after_tool_call",
	agent.PostTurn:        "post_turn",
}

// hookPointFromName 将协议蛇形命名法名称映射为 HookPoint 常量。
var hookPointFromName = map[string]agent.HookPoint{
	"before_model_call": agent.BeforeModelCall,
	"after_model_call":  agent.AfterModelCall,
	"before_tool_call":  agent.BeforeToolCall,
	"after_tool_call":   agent.AfterToolCall,
	"post_turn":         agent.PostTurn,
}

// syncHookPoints 是需要等待插件响应的同步钩子点。
var syncHookPoints = map[agent.HookPoint]bool{
	agent.BeforeModelCall: true,
	agent.BeforeToolCall:  true,
}

const hookCallTimeout = 10 * time.Second

// RegisterPluginHooks 查询钩子插件所需的钩子点，并在代理的钩子注册表中
// 注册将事件转发给插件的 HookFunc。
func RegisterPluginHooks(ctx context.Context, mgr *Manager, pluginID string, registry *agent.HookRegistry, agentName string) error {
	inst := mgr.Plugin(pluginID)
	if inst == nil || inst.Process == nil || !inst.Process.IsRunning() {
		return fmt.Errorf("plugin %s not running", pluginID)
	}

	// 询问插件它需要哪些钩子点
	result, err := inst.Process.Call(ctx, MethodHookRegister, nil)
	if err != nil {
		return fmt.Errorf("hook.register call to %s: %w", pluginID, err)
	}

	var reg HookRegisterResult
	if err := json.Unmarshal(result, &reg); err != nil {
		return fmt.Errorf("parse hook.register response from %s: %w", pluginID, err)
	}

	for _, pointName := range reg.Points {
		hp, ok := hookPointFromName[pointName]
		if !ok {
			slog.Warn("plugin: unknown hook point", "plugin", pluginID, "point", pointName)
			continue
		}

		// 捕获循环变量
		capturedHP := hp
		capturedPointName := pointName
		proc := inst.Process

		registry.Register(capturedHP, func(ctx context.Context, hc *agent.HookContext) {
			params := buildHookFireParams(capturedPointName, hc)

			if syncHookPoints[capturedHP] {
				// 同步：调用并等待修改后的消息
				callCtx, cancel := context.WithTimeout(ctx, hookCallTimeout)
				defer cancel()

				raw, err := proc.Call(callCtx, MethodHookFire, params)
				if err != nil {
					slog.Warn("plugin: hook.fire call failed",
						"plugin", pluginID, "point", capturedPointName, "error", err)
					return
				}

				var fireResult HookFireResult
				if err := json.Unmarshal(raw, &fireResult); err != nil {
					slog.Warn("plugin: hook.fire result parse failed",
						"plugin", pluginID, "point", capturedPointName, "error", err)
					return
				}

				// 如果插件返回了修改后的消息，则应用它们
				if len(fireResult.Messages) > 0 {
					hc.Messages = hookMessagesToProvider(fireResult.Messages)
				}
			} else {
				// 异步：发送后即忘记
				if err := proc.Notify(MethodHookFire, params); err != nil {
					slog.Warn("plugin: hook.fire notify failed",
						"plugin", pluginID, "point", capturedPointName, "error", err)
				}
			}
		})

		slog.Info("plugin: registered hook",
			"plugin", pluginID, "point", capturedPointName, "agent", agentName)
	}

	return nil
}

// buildHookFireParams 从 HookContext 构建 HookFireParams。
func buildHookFireParams(pointName string, hc *agent.HookContext) HookFireParams {
	params := HookFireParams{
		Point:      pointName,
		AgentName:  hc.AgentName,
		Channel:    hc.Channel,
		AccountID:  hc.AccountID,
		ChatID:     hc.ChatID,
		UserID:     hc.UserID,
		ToolName:   hc.ToolName,
		ToolArgs:   hc.ToolArgs,
		ToolResult: hc.ToolResult,
	}

	// 序列化消息
	if len(hc.Messages) > 0 {
		msgs := make([]HookMessage, 0, len(hc.Messages))
		for _, m := range hc.Messages {
			hm := HookMessage{
				Role:       m.Role,
				Content:    m.Content,
				ToolCallID: m.ToolCallID,
				Name:       m.Name,
			}
			if len(m.ToolCalls) > 0 {
				if tc, err := json.Marshal(m.ToolCalls); err == nil {
					hm.ToolCalls = tc
				}
			}
			msgs = append(msgs, hm)
		}
		params.Messages = msgs
	}

	// 序列化响应
	if hc.Response != nil {
		params.Response = &HookResponseData{
			Content:  hc.Response.Content,
			HasTools: hc.Response.HasToolCalls(),
		}
	}

	return params
}

// hookMessagesToProvider 将 HookMessage 转换回 provider.Message。
func hookMessagesToProvider(msgs []HookMessage) []provider.Message {
	result := make([]provider.Message, 0, len(msgs))
	for _, hm := range msgs {
		pm := provider.Message{
			Role:       hm.Role,
			Content:    hm.Content,
			ToolCallID: hm.ToolCallID,
			Name:       hm.Name,
		}
		if len(hm.ToolCalls) > 0 {
			var tcs []provider.ToolCall
			if err := json.Unmarshal(hm.ToolCalls, &tcs); err == nil {
				pm.ToolCalls = tcs
			}
		}
		result = append(result, pm)
	}
	return result
}
