package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/qs3c/bkcrab/internal/agent/goal"
)

// RegisterGoalTools 将单个模型可调用目标工具连接到 r 上。
//
// 只有 `update_goal(status="complete")` 暴露给模型：
// 继续提示已经为模型提供了当前目标 +
// 每回合都会进行预算，因此“get_goal”无需添加任何内容。我们
// 故意不让模型启动自己的目标（`create_goal`）
// 要么 - 目标是用户通过 /goal 斜线发起的。
//
// 状态仅限于模式层的“完整”字面意思。
// 暂停/恢复/预算限制是用户或运行时控制的，而不是
// 模型控制。镜像 codex-rs/core/src/tools/handlers/goal_spec.rs。
func RegisterGoalTools(r *Registry, st goal.Store, agentID string) {
	registerGoalToolsOn(r, st, agentID)
	// update_goal 捕获 registry 并读 r.GoalSessionKey()（每回合状态）。forTurn
	// 为每个回合克隆独立 registry 时，回放此钩子把工具重绑到回合私有副本，
	// 否则并发会话会共享同一个 goalSessionKey。
	r.onForTurn(func(rt *Registry) { registerGoalToolsOn(rt, st, agentID) })
}

func registerGoalToolsOn(r *Registry, st goal.Store, agentID string) {
	r.Register("update_goal",
		"Mark the active goal complete. Status is restricted to \"complete\"; "+
			"pausing, resuming, and budget_limited transitions are controlled by "+
			"the user or the runtime, not by the model. Only call this when the "+
			"objective has actually been achieved and no required work remains — "+
			"do not call it merely because the budget is nearly exhausted or "+
			"because you want to stop.",
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{
					"type":        "string",
					"enum":        []string{"complete"},
					"description": "Must be the literal string \"complete\".",
				},
			},
			"required":             []string{"status"},
			"additionalProperties": false,
		},
		makeUpdateGoal(st, r, agentID),
	)
}

// makeUpdateGoal 返回一个 ToolFunc，将活动目标翻转为
// 完全的。架构将状态限制为“完成”；我们重新验证
// 这里是防御性的，以防非 OpenAI 提供商通过
// 不合格模型响应。
func makeUpdateGoal(st goal.Store, r *Registry, agentID string) ToolFunc {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var a struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("update_goal: parse args: %w", err)
		}
		if a.Status != "complete" {
			return "", fmt.Errorf(
				"update_goal: status must be \"complete\"; pause / resume / budget_limited are user- or runtime-controlled, not model-controlled")
		}

		sessionKey := r.GoalSessionKey()
		if sessionKey == "" {
			return "", errors.New("update_goal: no active session context")
		}

		g, err := st.GetGoalBySession(ctx, agentID, sessionKey)
		if errors.Is(err, goal.ErrNotFound) {
			return "", errors.New("update_goal: no active goal for this session")
		}
		if err != nil {
			return "", fmt.Errorf("update_goal: load goal: %w", err)
		}
		if g.Status != goal.StatusActive {
			return "", fmt.Errorf("update_goal: goal status is %q; only an active goal can be marked complete", g.Status)
		}

		g.Status = goal.StatusComplete
		if err := st.UpdateGoal(ctx, g); err != nil {
			return "", fmt.Errorf("update_goal: %w", err)
		}
		b, _ := json.Marshal(map[string]any{
			"ok":                true,
			"final_token_usage": g.TokensUsed,
		})
		return string(b), nil
	}
}
