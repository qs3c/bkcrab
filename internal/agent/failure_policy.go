package agent

import "github.com/qs3c/bkcrab/internal/provider"

// 连续失败只触发调整方法的提醒，不代表所有工具不可用。
// 每段连续失败只提醒一次；任意成功调用都会重置。
type failureReminder struct {
	rounds int
}

func (f *failureReminder) observe(allFailed bool) bool {
	if !allFailed {
		f.rounds = 0
		return false
	}
	if f.rounds >= 3 {
		return false
	}
	f.rounds++
	return f.rounds == 3
}

func toolFailureReminder() provider.Message {
	return provider.Message{Role: "system", Content: "The last 3 rounds of tool calls all failed. Tools remain available. Review the actual errors and change your approach: check paths, interpreter or dependencies, and access restrictions as relevant. Different failures can indicate progress; do not assume all tools or sources are unavailable. Avoid repeating unchanged failing calls. Continue if a useful next step remains; otherwise report what is blocked and what is still unverified."}
}

// 请求是否允许工具也必须约束响应处理。不能只移除工具定义，
// 然后又执行模型返回的原生调用或正文恢复出的调用。
func (a *Agent) prepareToolResponse(resp *provider.Response, toolsAllowed bool) {
	if toolsAllowed {
		a.maybeRecoverToolCalls(resp)
		return
	}
	if resp == nil {
		return
	}
	calls, residual := recoverToolCallsFromContent(resp.Content)
	if resp.HasToolCalls() || len(calls) > 0 || residual != resp.Content {
		resp.Content = residual
		resp.ToolCalls = nil
		resp.RawAssistant = nil
		if resp.Content == "" {
			resp.Content = "Tools are disabled for this final response. The requested additional tool calls were not executed; the task remains incomplete."
		}
	}
}
