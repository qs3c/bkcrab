package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/qs3c/bkclaw/internal/provider"
)

// subagentDefaultTimeout 限制单个子代理在循环中可花费的挂钟时间，
// 与父代理整体轮次的剩余时间无关。足够大以容纳约 15-20 次
// camoufox-cli 驱动的迭代（含冷启动）。小于 agentTurnTimeout，这样
// 并行扇出中一个慢子代理不会拖垮其他子代理；父代理 ctx 取消
// 仍会传播，真正被终止的父代理会终止所有子代理。
const subagentDefaultTimeout = 15 * time.Minute

// RunSubagent 实现 tools.SubagentRunner，使 delegate_task 工具
// 可以回调到 Agent 而不产生导入循环。
//
// 退出时始终发送 phase="done" 的 `subagent_progress` 事件（无论
// 成功、错误还是通过 defer 捕获的 panic），使前端的"正在委托"
// 指示器可以在串行子代理运行之间干净地清除——即使下一个子代理
// 不立即启动或父代理决定在发出另一个 delegate_task 之前处理
// 结果。
func (a *Agent) RunSubagent(ctx context.Context, task string, maxIterations int) (out string, err error) {
	defer func() {
		emitEvent(ctx, ChatEvent{Type: "subagent_progress", Data: map[string]any{
			"phase": "done",
		}})
	}()
	return a.runSubagentLoop(ctx, task, maxIterations)
}

// runSubagentLoop 是 delegate_task 使用的自包含 ReAct 循环。
//
// 与 HandleMessage 共享的：
//   - 父代理的 provider、模型、工具注册表和 SDK 引擎
//   - 相同的循环检测、全失败轮次禁用工具和达到上限强制交付模式
//
// 与 HandleMessage 故意不做的：
//   - 无会话持久化——子代理的工作消息存在于私有切片中，
//     永远不会触及 session_messages
//   - 无聊天事件发射——父代理的聊天 UI 仅看到
//     delegate_task 工具调用 + 最终 tool_result，而非子代理的
//     中间步骤
//   - 无钩子、无技能存储刷新、无压缩、无 runPostTurn
//   - 无斜杠命令/计划模式短路（调用者是父代理模型通过
//     delegate_task 工具，而非人类编写者）
//
// delegate_task 本身被子代理工具集过滤掉，因此子代理不能
// 再生成子代理（v1 嵌套限制）。
//
// 返回契约：所有"我们得到了内容"情况下返回合成的最终文本——
// 干净退出、达到上限强制交付或循环检测中止。
// 仅对管道故障（无 provider、Chat 调用期间的瞬态 API 错误）
// 返回非 nil 错误；调用者将其折叠到 tool_result 中，
// 使父代理可以做出反应。
func (a *Agent) runSubagentLoop(ctx context.Context, task string, maxIterations int) (string, error) {
	if a.provider == nil {
		return "", fmt.Errorf("agent has no provider configured")
	}
	if maxIterations <= 0 {
		maxIterations = a.maxToolIterations
	}
	if maxIterations <= 0 {
		maxIterations = 20
	}

	// 每个子代理获得自己的有界 ctx，使慢速兄弟不会
	// 耗尽并行扇出的其余部分。父代理取消仍然优先——
	// 我们是包装而非分离。
	subCtx, cancel := context.WithTimeout(ctx, subagentDefaultTimeout)
	defer cancel()
	ctx = subCtx

	systemPrompt := a.ctxBuilder.BuildSystemPrompt() + subagentSystemSuffix()
	messages := []provider.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: task},
	}

	// 从子代理工具集中过滤 delegate_task——v1 不允许嵌套。
	// 其他工具（web_fetch、exec、文件操作、MCP 等）原样传递。
	var toolDefs []provider.Tool
	for _, t := range a.registry.Definitions() {
		if t.Function.Name == "delegate_task" {
			continue
		}
		toolDefs = append(toolDefs, t)
	}

	type sig struct {
		name string
		hash [32]byte
	}
	var lastSig sig
	consecutiveCount := 0
	allFailedRounds := 0
	const failedRoundsLimit = 3

	for i := 0; i < maxIterations; i++ {
		slog.Info("subagent iteration",
			"agent", a.name,
			"iteration", i+1,
			"max", maxIterations,
		)
		// 向父代理的聊天流发送心跳，使 UI 可以向用户显示
		// 子代理正在取得进展及其位置。没有这个，delegate_task
		// 工具卡片在整个子代理运行期间（通常 5-15 分钟）
		// 看起来会卡住。我们在每次迭代开始时和工具执行之前
		// （带工具名称）发射事件，使用户既能看到"思考中"也能
		// 看到"正在运行 web_search"阶段。
		emitEvent(ctx, ChatEvent{Type: "subagent_progress", Data: map[string]any{
			"iteration": i + 1,
			"max":       maxIterations,
			"phase":     "thinking",
		}})

		callTools := toolDefs
		llmMsgs := messages
		if allFailedRounds >= failedRoundsLimit {
			slog.Warn("subagent disabling tools after consecutive failed rounds",
				"agent", a.name, "failed_rounds", allFailedRounds)
			callTools = nil
			llmMsgs = append(llmMsgs, provider.Message{
				Role: "system",
				Content: fmt.Sprintf(
					"The last %d rounds of tool calls all failed (HTTP 4xx/5xx or empty results). Stop calling tools and produce the deliverable from what you already gathered, with explicit gaps marked.",
					allFailedRounds,
				),
			})
		}

		resp, err := a.provider.Chat(ctx, llmMsgs, callTools, a.model, a.maxTokens, a.temperature)
		if err != nil {
			// 如果 ctx 本身已过期，父调用者有比流中间
			// "context deadline exceeded" 更有用的框架——显式
			// 呈现超时信息，使父代理可以决定用更窄的任务范围重试。
			if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				return "", fmt.Errorf(
					"subagent ran out of its %s wall-time budget at iteration %d — task was too large; the parent should retry with a tighter scope or lower max_iterations",
					subagentDefaultTimeout, i+1)
			}
			return "", fmt.Errorf("subagent chat failed at iteration %d: %w", i+1, err)
		}

		if !resp.HasToolCalls() {
			return resp.Content, nil
		}

		messages = append(messages, provider.Message{
			Role:         "assistant",
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			Thinking:     resp.Thinking,
			RawAssistant: resp.RawAssistant,
		})

		// 循环检测：与 HandleMessage 相同的形状但在私有状态上。
		loopDetected := false
		for _, tc := range resp.ToolCalls {
			s := sig{name: tc.Function.Name, hash: sha256.Sum256([]byte(tc.Function.Arguments))}
			if s.name == lastSig.name && s.hash == lastSig.hash {
				consecutiveCount++
			} else {
				consecutiveCount = 1
				lastSig = s
			}
			if consecutiveCount >= 3 {
				slog.Warn("subagent tool-loop detected", "agent", a.name, "tool", tc.Function.Name)
				messages = append(messages, provider.Message{
					Role:    "system",
					Content: "Loop detected: same tool with same arguments 3 times. Stop and produce the deliverable from what you have.",
				})
				loopDetected = true
				break
			}
		}
		if loopDetected {
			break
		}

		// 第二次心跳：工具即将运行。展示它们的名称，
		// 使 UI 可以显示"正在运行 web_search"/"正在运行
		// exec (camoufox-cli open …)"而不仅仅是一个加载指示器。
		toolNames := make([]string, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			toolNames = append(toolNames, tc.Function.Name)
		}
		emitEvent(ctx, ChatEvent{Type: "subagent_progress", Data: map[string]any{
			"iteration": i + 1,
			"max":       maxIterations,
			"phase":     "running",
			"tools":     toolNames,
		}})

		results := a.engine.executeToolsConcurrently(ctx, a.registry, resp.ToolCalls, a.workspacePath)
		roundAllFailed := true
		for idx, r := range results {
			tc := resp.ToolCalls[idx]
			resultContent, _ := extractToolMeta(r.result)
			if !isFailedToolResult(r.err, resultContent) {
				roundAllFailed = false
			}
			messages = append(messages, provider.Message{
				Role:       "tool",
				Content:    resultContent,
				ToolCallID: tc.ID,
				Name:       r.toolName,
			})
		}
		if roundAllFailed {
			allFailedRounds++
		} else {
			allFailedRounds = 0
		}
	}

	// 达到上限——关闭工具的强制交付轮次。与 HandleMessage
	// 相同的催促；系统消息在两种上下文中都读起来很自然。
	slog.Warn("subagent max iterations reached — forcing final delivery",
		"agent", a.name, "max", maxIterations)
	emitEvent(ctx, ChatEvent{Type: "subagent_progress", Data: map[string]any{
		"iteration": maxIterations,
		"max":       maxIterations,
		"phase":     "final-delivery",
	}})
	finalMessages := append(messages, capReachedNudge(maxIterations))
	finalResp, err := a.provider.Chat(ctx, finalMessages, nil, a.model, a.maxTokens, a.temperature)
	if err != nil {
		return "", fmt.Errorf("subagent forced final delivery failed: %w", err)
	}
	if finalResp.Content == "" {
		return fmt.Sprintf("[subagent reached %d-iteration limit without producing a final answer]", maxIterations), nil
	}
	return finalResp.Content, nil
}

// subagentSystemSuffix 附加到代理的常规系统提示词后面，
// 在 runSubagentLoop 下运行时使用。阐明契约：回复是父代理的
// 工具结果，不是与人类的聊天。没有这个，子代理持续产生
// 唠叨的"嗨！我来帮你找……"前文，父代理之后不得不在
// 拼接前剥离它们。
func subagentSystemSuffix() string {
	return "\n\n# Subagent mode\n\n" +
		"You are running as a delegated sub-agent invoked by a parent " +
		"agent via the `delegate_task` tool. Your reply is consumed as a " +
		"tool result, not displayed to a human as chat. Follow these " +
		"rules strictly:\n\n" +
		"- Output **only** the deliverable the task asks for. No " +
		"preamble (\"Sure, I'll help…\"), no reassurance, no follow-up " +
		"questions, no offers to continue.\n" +
		"- If the task specifies an output format (table, JSON, " +
		"markdown rows), produce exactly that format — the parent " +
		"splices your output into a larger result.\n" +
		"- If you can't complete the task, return a brief note " +
		"explaining what you got and what blocked you. Partial " +
		"structured output beats no output.\n" +
		"- You have the parent's full tool set except `delegate_task` " +
		"itself (no nesting). Use them as normal.\n" +
		"- You don't see the parent's prior conversation. Everything " +
		"you need to do this task is in the user message below."
}
