package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
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
	// Workspace mirrors learner CRUD into <agent>/learner-skills. A nil store
	// keeps the single-node/local-only behaviour.
	Workspace workspace.Store
	// AllowDelete=false 时拒绝 delete 动作——后台提取循环禁止删技能,
	// 删除只归主对话(owner)与生命周期清理。
	AllowDelete bool
}

type skillManageArgs struct {
	Action  string `json:"action"`
	Slug    string `json:"slug,omitempty"`
	Content string `json:"content,omitempty"`
}

const skillManageDescription = "Manage only this agent's learner-generated skill library (SKILL.md files): create, read, update, delete, or list learner skills. It does not manage installed, manually created, or personal skills. Use when the owner asks to maintain an automatically learned workflow, or when a proven reusable workflow is worth extracting."

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

// SkillManageToolDef returns the same provider tool schema to the background
// extraction loop. The model still acts through a structured skill_manage
// ToolCall; only the chat-specific Registry authorization wrapper is absent.
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

// SkillManageExec returns the structured skill_manage tool executor used by
// the background learner to dispatch model ToolCalls. This is still the tool
// path (schema -> ToolCall -> executor), not a direct Manager call. Background
// owner authorization happens before the learner loop starts; the main-chat
// path additionally enforces its per-turn gate in registerSkillManage.
func SkillManageExec(deps SkillManageDeps) ToolFunc {
	return makeSkillManage(func() SkillManageDeps { return deps }, nil)
}

// makeSkillManage 构造 skill_manage 的 ToolFunc。deps 以函数注入按调用时
// 求值——builtin 注册发生在 SetSkillManage 之前,晚装配的管理器/账本因此
// 无需重新注册即可生效。gate 非 nil 时所有动作先过门控。
func makeSkillManage(deps func() SkillManageDeps, gate func(action string) error) ToolFunc {
	return func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args skillManageArgs
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		action := strings.ToLower(strings.TrimSpace(args.Action))
		if gate != nil {
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
		if err := syncLearnerSkill(ctx, deps, slug); err != nil {
			localRollback := deps.Manager.Delete(slug)
			remoteRollback := deleteLearnerSkill(ctx, deps, slug)
			return "", fmt.Errorf("persist created skill %q: %w", slug, errors.Join(err, localRollback, remoteRollback))
		}
		upsertSkillLedger(ctx, deps, slug, content, true)
		return fmt.Sprintf("created skill %q", slug), nil
	case "update":
		if slug == "" || strings.TrimSpace(content) == "" {
			return "", fmt.Errorf("update requires slug and content")
		}
		previous, ok := deps.Manager.Read(slug)
		if !ok {
			return "", fmt.Errorf("skill %q does not exist", slug)
		}
		if err := deps.Manager.Update(slug, content); err != nil {
			return "", err
		}
		if err := syncLearnerSkill(ctx, deps, slug); err != nil {
			localRollback := deps.Manager.Update(slug, previous)
			var remoteRollback error
			if localRollback == nil {
				remoteRollback = syncLearnerSkill(ctx, deps, slug)
			}
			return "", fmt.Errorf("persist updated skill %q: %w", slug, errors.Join(err, localRollback, remoteRollback))
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
		_, ok := deps.Manager.Read(slug)
		if !ok {
			return "", fmt.Errorf("skill %q does not exist", slug)
		}
		if err := deleteLearnerSkill(ctx, deps, slug); err != nil {
			return "", fmt.Errorf("delete remote skill %q: %w", slug, err)
		}
		if err := deps.Manager.Delete(slug); err != nil {
			// The local copy still exists, so best-effort restore the remote copy
			// before surfacing the error. Hydration must not turn a local failure
			// into an implicit successful delete on the next turn.
			remoteRollback := syncLearnerSkill(ctx, deps, slug)
			return "", fmt.Errorf("delete local skill %q: %w", slug, errors.Join(err, remoteRollback))
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

func syncLearnerSkill(ctx context.Context, deps SkillManageDeps, slug string) error {
	if deps.Workspace == nil {
		return nil
	}
	if deps.AgentID == "" {
		return fmt.Errorf("agent id is required for learner skill persistence")
	}
	return skills.SyncLearnerSkillUp(ctx, deps.Workspace, deps.AgentID, slug, deps.Manager.RootDir())
}

func deleteLearnerSkill(ctx context.Context, deps SkillManageDeps, slug string) error {
	if deps.Workspace == nil {
		return nil
	}
	if deps.AgentID == "" {
		return fmt.Errorf("agent id is required for learner skill persistence")
	}
	return skills.DeleteLearnerSkillUp(ctx, deps.Workspace, deps.AgentID, slug)
}

// registerSkillManage 把 skill_manage 注册为 builtin。依赖按调用时从
// Registry 字段读取;所有动作过 owner 门控。ForTurn 重新注册 builtins,
// 闭包因此捕获回合副本、读到本回合的 chatterUserID。
func registerSkillManage(r *Registry) {
	r.Register("skill_manage", skillManageDescription, skillManageSchema(), makeSkillManage(
		func() SkillManageDeps {
			return SkillManageDeps{
				Manager:     r.skillManager,
				Upserter:    r.skillLedger,
				Deleter:     r.skillLedger,
				AgentID:     r.agentID,
				Workspace:   r.workspaceStore,
				AllowDelete: true,
			}
		},
		func(action string) error {
			if r.skillManageAllowed {
				return nil
			}
			return fmt.Errorf("skill management is restricted to the agent owner")
		},
	))
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
