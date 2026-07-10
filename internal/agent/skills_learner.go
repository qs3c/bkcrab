package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/qs3c/bkcrab/internal/agent/tools"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
	"github.com/qs3c/bkcrab/internal/workspace"
)

type SkillsLearner struct {
	workspace      string
	provider       provider.Provider
	model          string
	minToolCalls   int
	skillDirs      []string
	manager        *skills.Manager
	agentID        string
	ledger         tools.SkillLedgerUpserter
	workspaceStore workspace.Store
}

func NewSkillsLearner(workspace string, p provider.Provider, model string, skillDirs ...string) *SkillsLearner {
	return &SkillsLearner{
		workspace:    workspace,
		provider:     p,
		model:        model,
		minToolCalls: 10,
		skillDirs:    skillDirs,
		manager:      skills.NewManager(skills.LearnerSkillsDir(workspace), skills.DefaultManagerConfig()),
	}
}

func (sl *SkillsLearner) Manager() *skills.Manager {
	if sl == nil {
		return nil
	}
	return sl.manager
}

// MaybeExtract checks a single turn for reusable skill extraction.
// The persistent cadence path enforces the threshold in ClaimSkillBatch.
func (sl *SkillsLearner) MaybeExtract(ctx context.Context, messages []provider.Message, toolCallCount int) error {
	if toolCallCount < sl.minToolCalls {
		return nil
	}
	return sl.extractWithTools(ctx, renderProviderMessages(messages))
}

// ExtractFromSession 从已认领并回放的 owner turns 提取技能。调用方负责按
// agent owner 过滤归档素材；这里保留每条消息、工具参数和结果的全文，不再
// 读取可能混入 guest 内容的 sessions.messages 工作集快照。非 nil 错误代表
// 基础设施故障(调用方重置批次)；校验/扫描拒绝在工具循环内反馈给模型消化。
func (sl *SkillsLearner) ExtractFromSession(ctx context.Context, msgs []store.SessionMessage) error {
	material := renderSessionMessages(msgs)
	if strings.TrimSpace(material) == "" {
		return nil
	}
	return sl.extractWithTools(ctx, material)
}

// skillExtractMaxIterations 限制提取工具循环轮数:新建 1 轮;撞 slug 走
// read→update 2-3 轮;再留一轮给校验拒绝后的自我修正。达到上限时已落盘
// 的写入保留,循环静默收尾。
const skillExtractMaxIterations = 4

// extractWithTools 把会话素材交给提取模型,模型通过 skill_manage 工具直接
// 落盘(create/update/read/list;delete 被禁)。相比旧的裸 JSON 输出+二次合并
// 调用:结构化工具调用不怕 markdown 围栏;撞 slug 时模型能 read 现有技能全文
// 再合并(旧路径只有名字+一句话描述);校验/安全扫描拒绝作为工具结果反馈给
// 模型修正,而非静默丢弃。无工具调用的回复即"无可提取"或"已完成"。
// 非 nil 返回值只代表基础设施故障(Chat 调用失败),调用方据此重置批次。
func (sl *SkillsLearner) extractWithTools(ctx context.Context, material string) error {
	prompt := sl.loadSkillLearnerPrompt()
	if existing := sl.existingSkillsPrompt(); existing != "" {
		prompt += "\n\n" + existing
	}
	messages := []provider.Message{
		{Role: "system", Content: prompt},
		{Role: "user", Content: material},
	}
	toolDefs := []provider.Tool{tools.SkillManageToolDef()}
	exec := tools.SkillManageExec(tools.SkillManageDeps{
		Manager:     sl.manager,
		Upserter:    sl.ledger,
		AgentID:     sl.agentID,
		Workspace:   sl.workspaceStore,
		AllowDelete: false,
	})
	for i := 0; i < skillExtractMaxIterations; i++ {
		resp, err := sl.provider.Chat(ctx, messages, toolDefs, sl.model, 4096, 0.3)
		if err != nil {
			return err
		}
		if len(resp.ToolCalls) == 0 {
			return nil
		}
		messages = append(messages, provider.Message{
			Role:         "assistant",
			Content:      resp.Content,
			ToolCalls:    resp.ToolCalls,
			Thinking:     resp.Thinking,
			RawAssistant: resp.RawAssistant,
		})
		for _, tc := range resp.ToolCalls {
			var result string
			if tc.Function.Name != "skill_manage" {
				result = fmt.Sprintf("error: unknown tool %q — only skill_manage is available", tc.Function.Name)
			} else if out, execErr := exec(ctx, json.RawMessage(tc.Function.Arguments)); execErr != nil {
				result = "error: " + execErr.Error()
			} else {
				result = out
				var logArgs struct {
					Action string `json:"action"`
					Slug   string `json:"slug"`
				}
				_ = json.Unmarshal([]byte(tc.Function.Arguments), &logArgs)
				slog.Info("skill extraction action applied",
					"agent", sl.agentID,
					"action", strings.ToLower(strings.TrimSpace(logArgs.Action)),
					"slug", strings.TrimSpace(logArgs.Slug),
				)
			}
			messages = append(messages, provider.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
			})
		}
	}
	slog.Debug("skill extraction reached iteration cap", "max", skillExtractMaxIterations)
	return nil
}

func (sl *SkillsLearner) loadSkillLearnerPrompt() string {
	for _, dir := range sl.skillDirs {
		path := filepath.Join(dir, "bkcrab-skill-learner", "SKILL.md")
		if data, err := os.ReadFile(path); err == nil {
			slog.Debug("loaded skill-learner prompt from file", "path", path)
			return string(data)
		}
	}
	return fallbackExtractionPrompt
}

const fallbackExtractionPrompt = `You maintain this agent's learner-generated skill library. Analyze the supplied owner turns and decide whether they demonstrate a reusable multi-step skill worth saving, acting through the skill_manage tool.

The input is a batch of completed turns replayed verbatim from one agent owner's archive, including tool calls, arguments, and results. It may be only part of a longer session; infer only what the supplied evidence supports.

Save a skill when the conversation shows at least one of:
- A repeatable multi-step workflow: multiple tool calls in a clear sequence, general enough to be useful in other contexts
- A hard-won approach: the task required trial and error, or the course changed because of findings along the way — capture the path that worked and the dead ends to avoid
- An expectation correction: the user expected a different method or outcome than the first attempt

How to act:
- New skill: call skill_manage {action:"create", slug:"kebab-case-slug", content:"..."}. content is a full SKILL.md: YAML frontmatter with non-empty name and description, then step-by-step markdown instructions.
- A listed existing skill covers the same workflow: first call {action:"read", slug} to see its current content, then {action:"update", slug, content} with a merged version keeping the best of both. Skip the update if the existing skill already covers everything this conversation taught.
- If a call is rejected, fix the content per the error message and retry once.
- Nothing worth saving: do not call any tool; reply with the single line: Nothing to save.`

// 两个渲染函数都不截断:工作流细节(工具参数、长结果)是 SOP 提取的
// 主体,截断会掏空技能内容。素材上界由上下文压缩天然保证——工作集
// 本身就是发给主模型的内容,提取模型的上下文窗口不小于主模型即可。

func renderProviderMessages(messages []provider.Message) string {
	var sb strings.Builder
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("  -> tool: %s(%s)\n", tc.Function.Name, tc.Function.Arguments))
		}
	}
	return sb.String()
}

func renderSessionMessages(msgs []store.SessionMessage) string {
	var sb strings.Builder
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, m.Content))
		if m.ToolCalls != nil {
			if b, err := json.Marshal(m.ToolCalls); err == nil {
				if s := string(b); s != "null" && s != `""` && s != "[]" {
					sb.WriteString(fmt.Sprintf("  -> tools: %s\n", s))
				}
			}
		}
	}
	return sb.String()
}

func (sl *SkillsLearner) existingSkillsPrompt() string {
	if sl == nil || sl.manager == nil {
		return ""
	}
	items := sl.manager.List()
	if len(items) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Existing learner-managed skills for this agent. If a newly extracted skill is substantially similar to one of these, read it and update that slug instead of creating a duplicate:\n")
	for _, item := range items {
		desc := firstSentence(item.Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&sb, "- %s - %s\n", item.Slug, desc)
	}
	return sb.String()
}
