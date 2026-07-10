package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
)

// SkillLedgerUpserter / SkillLedgerDeleter 是 skill_manage 写路径同步的
// 生命周期账本子集。拆成单方法接口,让只做 create/update 的提取循环不必
// 伪造 Delete。store.Store 同时满足两者。
type SkillLedgerUpserter interface {
	UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error
}

type SkillLedgerDeleter interface {
	DeleteSkillUsage(ctx context.Context, agentID, slug string) error
}

// SkillManageLedger 是 Registry 装配用的账本全集(写 + 删)。
type SkillManageLedger interface {
	SkillLedgerUpserter
	SkillLedgerDeleter
}

// SkillManageDeps 打包 skill_manage 动作执行的依赖。
type SkillManageDeps struct {
	Manager  *skills.Manager
	Upserter SkillLedgerUpserter // 可为 nil(无 store 装配):跳过记账
	Deleter  SkillLedgerDeleter  // 可为 nil:delete 只删目录
	AgentID  string
	// AllowDelete=false 时拒绝 delete 动作——后台提取循环禁止删技能,
	// 删除只归主对话(owner)与生命周期清理。
	AllowDelete bool
}

type skillManageArgs struct {
	Action  string `json:"action"`
	Slug    string `json:"slug,omitempty"`
	Content string `json:"content,omitempty"`
}

const skillManageDescription = "Manage this agent's shared skill library (SKILL.md files): create a new skill, read/update/delete an existing one, or list all skills. Use when the user asks to save, change, or remove a skill, or when a proven reusable workflow is worth persisting."

func skillManageSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"action": map[string]any{
				"type":        "string",
				"enum":        []string{"create", "update", "read", "delete", "list"},
				"description": "create: add a new skill; update: overwrite an existing skill with merged content; read: return a skill's full SKILL.md; delete: remove a skill; list: enumerate all skills with descriptions.",
			},
			"slug": map[string]any{
				"type":        "string",
				"description": "Kebab-case skill identifier (e.g. \"deploy-go-service\"). Required for create/update/read/delete.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full SKILL.md content: YAML frontmatter with non-empty name and description, then step-by-step markdown instructions. Required for create/update.",
			},
		},
		"required": []string{"action"},
	}
}

// SkillManageToolDef 返回 skill_manage 的 provider 工具定义,供不经过
// Registry 的调用方(技能提取循环)把同一工具喂给 Chat。
func SkillManageToolDef() provider.Tool {
	return provider.Tool{
		Type: "function",
		Function: provider.ToolFunction{
			Name:        "skill_manage",
			Description: skillManageDescription,
			Parameters:  skillManageSchema(),
		},
	}
}

// SkillManageExec 返回无权限门控的 skill_manage 执行函数,供后台提取循环
// 直接分发模型的工具调用。主对话路径走 registerSkillManage(带 owner 门控)。
func SkillManageExec(deps SkillManageDeps) ToolFunc {
	return makeSkillManage(func() SkillManageDeps { return deps }, nil)
}

func isSkillWriteAction(action string) bool {
	switch action {
	case "create", "update", "delete":
		return true
	}
	return false
}

// makeSkillManage 构造 skill_manage 的 ToolFunc。deps 以函数注入按调用时
// 求值——builtin 注册发生在 SetSkillManage 之前,晚装配的管理器/账本因此
// 无需重新注册即可生效。gate 非 nil 时写动作先过门控。
func makeSkillManage(deps func() SkillManageDeps, gate func(action string) error) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args skillManageArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		action := strings.ToLower(strings.TrimSpace(args.Action))
		if gate != nil && isSkillWriteAction(action) {
			if err := gate(action); err != nil {
				return "", err
			}
		}
		return applySkillManage(ctx, deps(), action, strings.TrimSpace(args.Slug), args.Content)
	}
}

func applySkillManage(ctx context.Context, deps SkillManageDeps, action, slug, content string) (string, error) {
	if deps.Manager == nil {
		return "", fmt.Errorf("skill management is not configured on this agent")
	}
	switch action {
	case "create":
		if slug == "" || strings.TrimSpace(content) == "" {
			return "", fmt.Errorf("create requires slug and content")
		}
		if err := deps.Manager.Create(slug, content); err != nil {
			return "", err
		}
		upsertSkillLedger(ctx, deps, slug, content, true)
		return fmt.Sprintf("created skill %q", slug), nil
	case "update":
		if slug == "" || strings.TrimSpace(content) == "" {
			return "", fmt.Errorf("update requires slug and content")
		}
		if err := deps.Manager.Update(slug, content); err != nil {
			return "", err
		}
		upsertSkillLedger(ctx, deps, slug, content, false)
		return fmt.Sprintf("updated skill %q", slug), nil
	case "read":
		if slug == "" {
			return "", fmt.Errorf("read requires slug")
		}
		got, ok := deps.Manager.Read(slug)
		if !ok {
			return "", fmt.Errorf("skill %q not found", slug)
		}
		return got, nil
	case "delete":
		if !deps.AllowDelete {
			return "", fmt.Errorf("delete is not available in this context")
		}
		if slug == "" {
			return "", fmt.Errorf("delete requires slug")
		}
		if err := deps.Manager.Delete(slug); err != nil {
			return "", err
		}
		if deps.Deleter != nil && deps.AgentID != "" {
			if err := deps.Deleter.DeleteSkillUsage(ctx, deps.AgentID, slug); err != nil {
				slog.Warn("skill ledger delete failed", "slug", slug, "error", err)
			}
		}
		return fmt.Sprintf("deleted skill %q", slug), nil
	case "list":
		items := deps.Manager.List()
		if len(items) == 0 {
			return "(no skills)", nil
		}
		var sb strings.Builder
		for _, it := range items {
			fmt.Fprintf(&sb, "- %s — %s\n", it.Slug, it.Description)
		}
		return sb.String(), nil
	default:
		return "", fmt.Errorf("unknown action %q: use create, update, read, delete, or list", action)
	}
}

// upsertSkillLedger 尽力同步生命周期账本;记账失败只 Warn,绝不回滚已
// 落盘的技能——与 load_skill 的记账语义一致。
func upsertSkillLedger(ctx context.Context, deps SkillManageDeps, slug, content string, firstCreate bool) {
	if deps.Upserter == nil || deps.AgentID == "" {
		return
	}
	if err := deps.Upserter.UpsertSkillUsage(ctx, deps.AgentID, slug, store.HashSkillContent(content), firstCreate); err != nil {
		slog.Warn("skill ledger upsert failed", "slug", slug, "error", err)
	}
}
