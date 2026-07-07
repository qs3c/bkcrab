package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/skills"
	"github.com/qs3c/bkcrab/internal/store"
)

type SkillsLearner struct {
	workspace    string
	provider     provider.Provider
	model        string
	minToolCalls int
	skillDirs    []string
	manager      *skills.Manager
	agentID      string
	ledger       skillLedger
}

type skillLedger interface {
	UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error
}

func NewSkillsLearner(workspace string, p provider.Provider, model string, skillDirs ...string) *SkillsLearner {
	return &SkillsLearner{
		workspace:    workspace,
		provider:     p,
		model:        model,
		minToolCalls: 10,
		skillDirs:    skillDirs,
		manager:      skills.NewManager(filepath.Join(workspace, "skills"), skills.DefaultManagerConfig()),
	}
}

func (sl *SkillsLearner) Manager() *skills.Manager {
	if sl == nil {
		return nil
	}
	return sl.manager
}

type extractedSkill struct {
	Name        string `json:"name"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

type extractionResponse struct {
	Extract bool           `json:"extract"`
	Skill   extractedSkill `json:"skill"`
}

// MaybeExtract checks a single turn for reusable skill extraction.
// The persistent cadence path enforces the threshold in ClaimSkillBatch.
func (sl *SkillsLearner) MaybeExtract(ctx context.Context, messages []provider.Message, toolCallCount int) error {
	if toolCallCount < sl.minToolCalls {
		return nil
	}

	skill, err := sl.extractFromSummary(ctx, summarizeProviderMessages(messages))
	if err != nil {
		return fmt.Errorf("extract skill: %w", err)
	}
	if skill == nil {
		return nil
	}
	return sl.persistExtracted(ctx, skill)
}

// ExtractFromTurns extracts a skill from archived turn groups claimed by cadence.
// Non-nil errors are infrastructure failures; validation/scan rejections are consumed.
func (sl *SkillsLearner) ExtractFromTurns(ctx context.Context, groups []store.TurnGroup) error {
	summary := summarizeTurnGroups(groups)
	if strings.TrimSpace(summary) == "" {
		return nil
	}
	skill, err := sl.extractFromSummary(ctx, summary)
	if err != nil {
		return fmt.Errorf("extract skill: %w", err)
	}
	if skill == nil {
		return nil
	}
	return sl.persistExtracted(ctx, skill)
}

func (sl *SkillsLearner) persistExtracted(ctx context.Context, skill *extractedSkill) error {
	if existing, ok := sl.manager.Read(skill.Slug); ok {
		merged, err := sl.decideUpdate(ctx, existing, skill)
		if err != nil {
			return fmt.Errorf("decide update: %w", err)
		}
		if merged == "" {
			slog.Debug("skill exists, update not needed", "slug", skill.Slug)
			return nil
		}
		if err := sl.manager.Update(skill.Slug, merged); err != nil {
			slog.Warn("skill update rejected", "slug", skill.Slug, "error", err)
			return nil
		}
		sl.upsertLedger(ctx, skill.Slug, merged, false)
		slog.Info("updated existing skill", "name", skill.Name, "slug", skill.Slug)
		return nil
	}

	if err := sl.manager.Create(skill.Slug, skill.Content); err != nil {
		slog.Warn("skill create rejected", "slug", skill.Slug, "error", err)
		return nil
	}
	sl.upsertLedger(ctx, skill.Slug, skill.Content, true)
	slog.Info("extracted new skill", "name", skill.Name, "slug", skill.Slug)
	return nil
}

func (sl *SkillsLearner) upsertLedger(ctx context.Context, slug, content string, firstCreate bool) {
	if sl == nil || sl.ledger == nil || sl.agentID == "" {
		return
	}
	if err := sl.ledger.UpsertSkillUsage(ctx, sl.agentID, slug, store.HashSkillContent(content), firstCreate); err != nil {
		slog.Warn("skill ledger upsert failed", "slug", slug, "error", err)
	}
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

const fallbackExtractionPrompt = `Analyze the following conversation and determine if it demonstrates a reusable multi-step skill.

Criteria for extraction:
- The task involved multiple tool calls in a clear, repeatable sequence
- The task is general enough to be useful in other contexts
- The steps can be described as a clear procedure

If this conversation demonstrates a reusable skill, output JSON:
{"extract": true, "skill": {"name": "Human readable name", "slug": "kebab-case-slug", "description": "One line description", "content": "Full SKILL.md content with YAML frontmatter"}}

If not reusable, output: {"extract": false}

The SKILL.md format uses YAML frontmatter:
---
name: Skill Name
description: What it does
---
Step-by-step instructions in markdown...

Output ONLY the JSON, no markdown fences.`

func summarizeProviderMessages(messages []provider.Message) string {
	var sb strings.Builder
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		content := truncate(m.Content, 500)
		sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, content))
		for _, tc := range m.ToolCalls {
			sb.WriteString(fmt.Sprintf("  -> tool: %s(%s)\n", tc.Function.Name, truncate(tc.Function.Arguments, 200)))
		}
	}
	return sb.String()
}

func summarizeTurnGroups(groups []store.TurnGroup) string {
	var sb strings.Builder
	for _, g := range groups {
		for _, m := range g.Messages {
			if m.Role == "system" {
				continue
			}
			content := truncate(m.Content, 500)
			sb.WriteString(fmt.Sprintf("[%s]: %s\n", m.Role, content))
			if m.ToolCalls != nil {
				if b, err := json.Marshal(m.ToolCalls); err == nil {
					if s := string(b); s != "null" && s != `""` && s != "[]" {
						sb.WriteString(fmt.Sprintf("  -> tools: %s\n", truncate(s, 300)))
					}
				}
			}
		}
	}
	return sb.String()
}

func (sl *SkillsLearner) extractFromSummary(ctx context.Context, summary string) (*extractedSkill, error) {
	prompt := sl.loadSkillLearnerPrompt()
	if existing := sl.existingSkillsPrompt(); existing != "" {
		prompt += "\n\n" + existing
	}
	extractMsgs := []provider.Message{
		{Role: "system", Content: prompt + "\n\nOutput ONLY the JSON, no markdown fences."},
		{Role: "user", Content: summary},
	}

	resp, err := sl.provider.Chat(ctx, extractMsgs, nil, sl.model, 4096, 0.3)
	if err != nil {
		return nil, err
	}

	var result extractionResponse
	content := strings.TrimSpace(resp.Content)
	if err := json.Unmarshal([]byte(content), &result); err != nil {
		slog.Debug("skill extraction: LLM response not valid JSON", "error", err)
		return nil, nil
	}
	if !result.Extract || result.Skill.Slug == "" || result.Skill.Content == "" {
		return nil, nil
	}
	return &result.Skill, nil
}

func (sl *SkillsLearner) existingSkillsPrompt() string {
	if sl == nil || sl.workspace == "" {
		return ""
	}
	found := discoverSkillsEnhanced(filepath.Join(sl.workspace, "skills"), "agent")
	if len(found) == 0 {
		return ""
	}
	names := make([]string, 0, len(found))
	for name := range found {
		names = append(names, name)
	}
	sort.Strings(names)
	var sb strings.Builder
	sb.WriteString("Existing skills in this workspace. If a newly extracted skill is substantially similar to one of these, reuse that slug so the existing skill is updated instead of creating a duplicate:\n")
	for _, name := range names {
		desc := firstSentence(found[name].Description)
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&sb, "- %s - %s\n", name, desc)
	}
	return sb.String()
}

const updateDecisionPrompt = `You maintain a library of agent skills (SKILL.md files).
An existing skill and a newly extracted skill share the same slug.
Decide whether the existing skill should be updated with what the new extraction learned.

Update when the new extraction adds real value: missing steps, corrections, pitfalls, broader applicability.
Do NOT update when the existing skill already covers the workflow adequately.

If updating, output JSON:
{"update": true, "content": "full merged SKILL.md content with YAML frontmatter"}
The content must merge the best of both versions and keep valid YAML frontmatter with name and description.

If not updating, output: {"update": false}

Output ONLY the JSON, no markdown fences.`

type updateDecision struct {
	Update  bool   `json:"update"`
	Content string `json:"content"`
}

func (sl *SkillsLearner) decideUpdate(ctx context.Context, existing string, skill *extractedSkill) (string, error) {
	user := fmt.Sprintf("EXISTING SKILL:\n%s\n\nNEWLY EXTRACTED SKILL:\n%s", existing, skill.Content)
	msgs := []provider.Message{
		{Role: "system", Content: updateDecisionPrompt},
		{Role: "user", Content: user},
	}
	resp, err := sl.provider.Chat(ctx, msgs, nil, sl.model, 4096, 0.3)
	if err != nil {
		return "", err
	}

	var dec updateDecision
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp.Content)), &dec); err != nil {
		slog.Debug("skill update decision: LLM response not valid JSON", "error", err)
		return "", nil
	}
	if !dec.Update || strings.TrimSpace(dec.Content) == "" {
		return "", nil
	}
	return dec.Content, nil
}

func truncate(s string, n int) string {
	if n < 0 {
		n = 0
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i] + "..."
		}
		count++
	}
	return s
}
