package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codeany-ai/open-agent-sdk-go/costtracker"
	sdktools "github.com/codeany-ai/open-agent-sdk-go/tools"
	sdktypes "github.com/codeany-ai/open-agent-sdk-go/types"

	"github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/provider"
)

// readOnlyTools 列出可以安全并发执行的工具。
var readOnlyTools = map[string]bool{
	"read_file":                      true,
	"list_dir":                       true,
	"web_fetch":                      true,
	"web_search":                     true,
	"rag_search":                     true,
	"memory_search":                  true,
	"load_skill":                     true,
	"retrieve_compacted_tool_result": true,
}

// toolAdapter 将 BkCrab 工具包装为 SDK Tool 接口。
type toolAdapter struct {
	name        string
	description string
	params      interface{}
	fn          tools.ToolFunc
}

func (t *toolAdapter) Name() string        { return t.name }
func (t *toolAdapter) Description() string { return t.description }

func (t *toolAdapter) InputSchema() sdktypes.ToolInputSchema {
	// 将 BkCrab 参数 (interface{}) 转换为 SDK ToolInputSchema
	if t.params == nil {
		return sdktypes.ToolInputSchema{Type: "object"}
	}
	data, err := json.Marshal(t.params)
	if err != nil {
		return sdktypes.ToolInputSchema{Type: "object"}
	}
	var schema sdktypes.ToolInputSchema
	if err := json.Unmarshal(data, &schema); err != nil {
		return sdktypes.ToolInputSchema{Type: "object"}
	}
	return schema
}

func (t *toolAdapter) Call(ctx context.Context, input map[string]interface{}, tCtx *sdktypes.ToolUseContext) (*sdktypes.ToolResult, error) {
	// 将输入映射转换为 JSON 供 BkCrab 的 ToolFunc 使用
	argsJSON, err := json.Marshal(input)
	if err != nil {
		return &sdktypes.ToolResult{IsError: true, Error: err.Error()}, nil
	}

	result, err := t.fn(ctx, json.RawMessage(argsJSON))
	if err != nil {
		errText := result
		if errText != "" {
			errText += "\n"
		}
		errText += err.Error()
		return &sdktypes.ToolResult{
			IsError: true,
			Error:   errText,
			Content: []sdktypes.ContentBlock{{
				Type: sdktypes.ContentBlockText,
				Text: errText,
			}},
		}, nil
	}

	return &sdktypes.ToolResult{
		Content: []sdktypes.ContentBlock{{
			Type: sdktypes.ContentBlockText,
			Text: result,
		}},
	}, nil
}

func (t *toolAdapter) IsConcurrencySafe(input map[string]interface{}) bool {
	return readOnlyTools[t.name]
}

func (t *toolAdapter) IsReadOnly(input map[string]interface{}) bool {
	return readOnlyTools[t.name]
}

// sdkEngine 包装 SDK 组件以支持并发工具执行和成本追踪。
type sdkEngine struct {
	costTracker *costtracker.Tracker
}

// newSDKEngine 创建一个带成本追踪的新 SDK 引擎。
func newSDKEngine(sessionID string) *sdkEngine {
	return &sdkEngine{
		costTracker: costtracker.NewTracker(sessionID),
	}
}

// buildSDKRegistry 将 BkCrab 的工具注册表转换为 SDK 注册表。
func buildSDKRegistry(fcRegistry *tools.Registry) *sdktools.Registry {
	sdkReg := sdktools.NewRegistry()
	for _, def := range fcRegistry.Definitions() {
		fn := fcRegistry.GetFunc(def.Function.Name)
		if fn == nil {
			continue
		}
		sdkReg.Register(&toolAdapter{
			name:        def.Function.Name,
			description: def.Function.Description,
			params:      def.Function.Parameters,
			fn:          fn,
		})
	}
	return sdkReg
}

// toolCallResult 保存单个工具调用的结果及元数据。
type toolCallResult struct {
	toolCallID string
	toolName   string
	result     string
	err        error
}

// executeToolsConcurrently 使用 SDK 的并发执行器运行工具调用。
func (e *sdkEngine) executeToolsConcurrently(ctx context.Context, fcRegistry *tools.Registry, toolCalls []provider.ToolCall, workspace string) []toolCallResult {
	sdkReg := buildSDKRegistry(fcRegistry)
	executor := sdktools.NewExecutor(sdkReg, nil, &sdktypes.ToolUseContext{
		WorkingDir: workspace,
		AbortCtx:   ctx,
	})

	// 将 BkCrab 工具调用转换为 SDK 格式
	calls := make([]sdktools.ToolCallRequest, len(toolCalls))
	for i, tc := range toolCalls {
		var input map[string]interface{}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
			input = map[string]interface{}{"_raw": tc.Function.Arguments}
		}
		calls[i] = sdktools.ToolCallRequest{
			ToolUseID: tc.ID,
			ToolName:  tc.Function.Name,
			Input:     input,
		}
	}

	start := time.Now()
	responses := executor.RunTools(ctx, calls)
	e.costTracker.AddToolDuration(time.Since(start))

	// Anthropic（和 OpenAI）需要每个工具都有一个工具结果_使用
	// 刚刚发出的模型 — 孤立的 tool_use ID 进行下一个 API 调用
	// 返回 400 invalid_request_error。 SDK可以短路和
	// 返回的响应少于请求的响应（上下文取消、执行器
	// 因沙箱创建失败等而中毒），因此构建结果
	// 在 toolCalls 上键入切片并通过 ToolUseID 查找而不是压缩
	// 逐个位置。缺失条目成为明显的失败
	// tool_results 以便对话历史记录保持格式良好。
	byID := make(map[string]sdktools.ToolCallResponse, len(responses))
	for _, resp := range responses {
		byID[resp.ToolUseID] = resp
	}
	results := make([]toolCallResult, len(toolCalls))
	for i, tc := range toolCalls {
		resp, ok := byID[tc.ID]
		if !ok {
			results[i] = toolCallResult{
				toolCallID: tc.ID,
				toolName:   tc.Function.Name,
				result:     "tool execution did not return a result (sandbox or executor failure — check gateway logs)",
				err:        fmt.Errorf("no response from executor for tool_use %s", tc.ID),
			}
			continue
		}
		var resultText string
		if resp.Result != nil {
			if resp.Result.IsError {
				resultText = resp.Result.Error
				if resultText == "" && len(resp.Result.Content) > 0 {
					resultText = resp.Result.Content[0].Text
				}
				results[i] = toolCallResult{
					toolCallID: resp.ToolUseID,
					toolName:   toolCalls[i].Function.Name,
					result:     resultText + "\n[Analyze the error above and try a different approach.]",
					err:        fmt.Errorf("%s", resultText),
				}
				continue
			}
			// 从内容块中提取文本
			var parts []string
			for _, cb := range resp.Result.Content {
				if cb.Text != "" {
					parts = append(parts, cb.Text)
				}
			}
			resultText = strings.Join(parts, "\n")
		}
		if resp.Error != nil {
			results[i] = toolCallResult{
				toolCallID: resp.ToolUseID,
				toolName:   toolCalls[i].Function.Name,
				result:     resultText,
				err:        resp.Error,
			}
		} else {
			results[i] = toolCallResult{
				toolCallID: resp.ToolUseID,
				toolName:   toolCalls[i].Function.Name,
				result:     resultText,
			}
		}
	}
	return results
}
