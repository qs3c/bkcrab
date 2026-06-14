package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// SubagentRunner 是 delegate_task 工具调用来生成
// 子代理。代理包在代理上实现了这一点，因此我们避免了
// 将代理拉入工具（将形成导入循环）。
type SubagentRunner interface {
	RunSubagent(ctx context.Context, task string, maxIterations int) (string, error)
}

type delegateTaskArgs struct {
	Task           string `json:"task"`
	ExpectedOutput string `json:"expected_output,omitempty"`
	MaxIterations  int    `json:"max_iterations,omitempty"`
}

// RegisterDelegateTask 连接 delegate_task 工具。跑步时无操作
// 为零，因此调用者可以通过简单地不构建一个来选择退出（例如
// 在测试或代理风格中，子代理扇出不会产生
// 感觉）。
//
// 注册为 SERIAL：两个 delegate_task 调用无法运行
// 在一名代理人内同时进行。子代理人共享母公司的单身
// 沙箱 + 单个 camoufox-cli 守护进程 — 并行运行 5 个进程
// 他们践踏了彼此的浏览器导航状态，又回来了
// 其他兄弟姐妹刚刚导航到并生成的页面快照
// 垃圾。序列化以扇出墙时间为代价（5 × N 分钟
// N 分钟）以获得实际正确的结果。
//
// 该工具说明解释了委托的原因（父母的
// 上下文保持干净，子代理获得新的迭代预算）并且
// 串行执行合约，因此模型不期望并行
// 扇出的吞吐量。 “禁止嵌套”这一行至关重要——没有
// 它的闪存层模型尝试递归地委托和烧穿
// 预算呈指数级增长。
func RegisterDelegateTask(r *Registry, runner SubagentRunner) {
	if runner == nil {
		return
	}
	r.RegisterSerial("delegate_task",
		"Spawn a sub-agent with its OWN context and OWN iteration budget to run a single bounded sub-task. "+
			"Use this when the user's request decomposes into several large independent chunks "+
			"(e.g. \"find 10 leads matching X\" then \"find another 10 matching Y\" then \"write 5 emails from this data\"). "+
			"Each sub-agent gets a fresh tool-iteration budget so you don't burn yours exploring, and your own context "+
			"stays clean of the dozens of intermediate tool results the sub-agent goes through. "+
			"\n\n**Sub-agents run SERIALLY, not in parallel.** Even if you emit 5 delegate_task calls in one round, "+
			"they execute one at a time — they share the single sandbox + single browser daemon, so parallel "+
			"execution would trample each other's state. Expect the wall-clock time of a fan-out to be N × the single-"+
			"sub-agent time, not 1× it. Plan accordingly: smaller per-sub-agent scope is better than fewer, larger calls.\n\n"+
			"The sub-agent runs against the same tools and provider you have (minus delegate_task itself — no nesting). "+
			"It cannot see your prior conversation, so pass everything it needs in the `task` arg: criteria, search hints, "+
			"earlier findings to build on, output format. Sub-agents are best for tasks that produce a self-contained "+
			"artifact (a table, a draft email, a structured summary). "+
			"\n\nReturn: the sub-agent's final text exactly as it produced it. You then assemble multiple sub-agent "+
			"results into the final deliverable for the user.",
		map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"task": map[string]interface{}{
					"type":        "string",
					"description": "Self-contained task description. The sub-agent does NOT see your prior conversation — include all the context it needs to act: criteria, search hints, prior findings it should build on, region / language constraints, anything the sub-agent must respect.",
				},
				"expected_output": map[string]interface{}{
					"type":        "string",
					"description": "Optional concrete format the sub-agent should produce — e.g. \"markdown table with columns: name, city, owner, phone, source_url; one row per business; no preamble\". Appended to the task verbatim so the format spec is unambiguous.",
				},
				"max_iterations": map[string]interface{}{
					"type":        "integer",
					"description": "Optional override for the sub-agent's tool-iteration budget. Default is the same cap as your turn (typically 20). REALISTIC BUDGETS: for browser-heavy sub-tasks (camoufox-cli — each open/snapshot/click is 1-30s real time, plus a 2-3 min cold-start on the first call), the sub-agent has a 15-minute wall-time cap, so a max_iterations of 12-18 is the practical ceiling — setting 40 just means it'll hit the wall-clock long before iteration 40 and you'll get a partial answer with a 'ran out of budget' note. For quick web_search / web_fetch sub-tasks, 20-30 is fine. For pure synthesis (no tools), 3-5 is enough.",
				},
			},
			"required": []string{"task"},
		},
		func(ctx context.Context, raw json.RawMessage) (string, error) {
			var args delegateTaskArgs
			if err := json.Unmarshal(raw, &args); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if args.Task == "" {
				return "", fmt.Errorf("task is required")
			}
			taskPrompt := args.Task
			if args.ExpectedOutput != "" {
				taskPrompt += "\n\n## Expected output format\n\n" + args.ExpectedOutput
			}
			out, err := runner.RunSubagent(ctx, taskPrompt, args.MaxIterations)
			if err != nil {
				// 在 tool_result 中显示错误，以便父级
				// 将其视为正常的工具故障（获取“分析
				// 错误并尝试不同的方法”信封来自
				// 注册表）而不是硬工具执行错误。
				return fmt.Sprintf("[subagent failed: %s]", err.Error()), err
			}
			return out, nil
		})
}
