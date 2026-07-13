# skill_manage 工具化实施计划

> **已取代：** 本文记录历史实施过程，当前行为与不变量以 [Skill 自动提炼（自进化）Canonical 设计与实现](../../skill-self-evolution.md) 为准。

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 skills.Manager 与模型的交互从裸 JSON prompt 驱动改为原生工具调用：新增 `skill_manage` 工具（create/update/read/delete/list），注册给主对话 agent（写动作仅 owner），后台技能提取路径改为有界工具循环，删除两处 JSON 裸解析。

**Architecture:** 照 `memory` 工具的形态在 tools 包新增 builtin `skill_manage`，依赖（learner 的 skills.Manager + 生命周期账本 + agentID）作为 Registry 共享字段经 `SetSkillManage` 装配；ForTurn 重新注册 builtins 使闭包读到回合私有的 chatterUserID，owner 门控据此判定。提取路径不走 Registry：`SkillsLearner` 用 `tools.SkillManageExec`（无门控、禁 delete）直接分发模型的工具调用，循环 ≤4 轮。

**Tech Stack:** Go；internal/skills（Manager CRUD+校验+安全扫描，现成）、internal/agent/tools（Registry/builtin 模式）、internal/provider（Tool/ToolCall/Response 类型）。

**设计决策（已与用户定稿）：**
- 技能保持 **agent 级共享资产**；`skill_usage` 账本保持 `(agent_id, slug)` 键**原样不动**，零数据库变更。
- 主对话 `skill_manage` 写动作（create/update/delete）**仅 agent 所有者**；read/list 不限。
- 提取路径保持低成本：有界工具循环（新建 1 轮、read→update 2-3 轮、留 1 轮自我修正），**禁 delete**。
- 校验/安全扫描拒绝作为工具结果反馈给模型修正，不再静默丢弃。

**关键约束（背离即 bug）：**
- tools 包不得导入 agent 包（现有依赖方向：agent → tools → skills/provider/store）。
- Registry 新增字段必须同步加进 `ForTurn` 的共享依赖复制块（registry.go:577 维护提示）——漏掉则回合副本上工具失效，多会话并发出错。
- 生产 agent 走 `buildAgent`/manager.go 装配路径：`SetSkillManage` 必须在 manager.go 的 dataStore 补丁点调用，只接构造器会导致生产环境账本不同步（参见 manager.go:250-251 现有 learner 补丁）。

---

## 文件结构

| 文件 | 动作 | 职责 |
|---|---|---|
| `internal/skills/manager.go` | 修改 | 提炼 `parseFrontmatter`；新增 `Manager.List()` |
| `internal/skills/manager_list_test.go` | 新建 | List 的 TDD 测试 |
| `internal/agent/tools/skill_manage.go` | 新建 | 工具 schema/定义、动作分发核心、账本同步、builtin 注册 + owner 门控 |
| `internal/agent/tools/skill_manage_test.go` | 新建 | 动作核心 + 门控 + ForTurn 继承测试 |
| `internal/agent/tools/registry.go` | 修改 | 字段 `skillManager`/`skillLedger`、`SetSkillManage`、`skillWriteAllowed`、ForTurn 复制、registerBuiltins 挂接 |
| `internal/agent/skills_learner.go` | 修改 | 提取改工具循环；删除 JSON 路径全部符号；重写 fallback 提示词 |
| `internal/agent/skills_learner_test.go` | 修改 | fake provider 改为脚本化工具调用响应；测试全面重写 |
| `internal/agent/loop.go` | 修改 | 构造器处 `SetSkillManage` 接线 |
| `internal/agent/manager.go` | 修改 | 生产装配处 `SetSkillManage(mgr, dataStore)` 接线 |
| `skills/bkcrab-skill-learner/SKILL.md` | 修改 | 提示词从 JSON 输出格式改为 skill_manage 工具用法 |

---

### Task 1: skills.Manager 增加 List（含 parseFrontmatter 提炼）

**Files:**
- Modify: `internal/skills/manager.go`
- Test: `internal/skills/manager_list_test.go`（新建）

- [ ] **Step 1: 写失败测试**

新建 `internal/skills/manager_list_test.go`：

```go
package skills

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListMissingRootReturnsEmpty(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "none"), DefaultManagerConfig())
	if got := m.List(); len(got) != 0 {
		t.Fatalf("List() on missing root = %v, want empty", got)
	}
}

func TestListReturnsSkillsSortedWithMetadata(t *testing.T) {
	m := NewManager(t.TempDir(), DefaultManagerConfig())
	if err := m.Create("zeta-skill", "---\nname: Zeta\ndescription: does z\n---\nbody\n"); err != nil {
		t.Fatal(err)
	}
	if err := m.Create("alpha-skill", "---\nname: Alpha\ndescription: does a\n---\nbody\n"); err != nil {
		t.Fatal(err)
	}
	got := m.List()
	if len(got) != 2 || got[0].Slug != "alpha-skill" || got[1].Slug != "zeta-skill" {
		t.Fatalf("List() = %+v, want [alpha-skill zeta-skill]", got)
	}
	if got[0].Name != "Alpha" || got[0].Description != "does a" {
		t.Fatalf("List()[0] = %+v, want frontmatter metadata", got[0])
	}
}

func TestListSkipsBrokenFrontmatter(t *testing.T) {
	root := t.TempDir()
	m := NewManager(root, DefaultManagerConfig())
	if err := m.Create("good-skill", "---\nname: Good\ndescription: ok\n---\nbody\n"); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(root, "bad-skill")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("no frontmatter"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := m.List()
	if len(got) != 1 || got[0].Slug != "good-skill" {
		t.Fatalf("List() = %+v, want only good-skill", got)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/skills/ -run TestList -v`
Expected: 编译失败 `undefined: SkillListItem` / `m.List undefined`

- [ ] **Step 3: 最小实现**

在 `internal/skills/manager.go`：

(a) 从 `validateContent` 提炼 frontmatter 解析（保持两处判定一致）。用下面的函数**替换** `validateContent` 中 `if !strings.HasPrefix(...)` 到 `body := ...` 的解析段：

```go
// parseFrontmatter 解析 SKILL.md 的 YAML frontmatter,返回 frontmatter 与正文。
// 与 validateContent 共用,保证 List 与写入校验对"合法 frontmatter"判定一致。
func parseFrontmatter(content string) (skillFrontmatter, string, error) {
	var fm skillFrontmatter
	if !strings.HasPrefix(content, "---\n") {
		return fm, "", errors.New("SKILL.md must start with YAML frontmatter (---)")
	}
	rest := strings.TrimPrefix(content, "---\n")
	const frontmatterEnd = "\n---\n"
	end := strings.Index(rest, frontmatterEnd)
	if end < 0 {
		return fm, "", errors.New("SKILL.md frontmatter is not closed with ---")
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return fm, "", fmt.Errorf("frontmatter parse error: %w", err)
	}
	return fm, rest[end+len(frontmatterEnd):], nil
}
```

`validateContent` 改为：

```go
func (m *Manager) validateContent(content string) error {
	if utf8.RuneCountInString(content) > m.config.MaxContentChars {
		return fmt.Errorf("skill content exceeds %d chars", m.config.MaxContentChars)
	}
	fm, body, err := parseFrontmatter(content)
	if err != nil {
		return err
	}
	if strings.TrimSpace(fm.Name) == "" {
		return errors.New("frontmatter must include non-empty 'name'")
	}
	if strings.TrimSpace(fm.Description) == "" {
		return errors.New("frontmatter must include non-empty 'description'")
	}
	if utf8.RuneCountInString(fm.Description) > m.config.MaxDescriptionChars {
		return fmt.Errorf("description exceeds %d chars", m.config.MaxDescriptionChars)
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("SKILL.md must have content after the frontmatter")
	}
	return nil
}
```

(b) 新增 List（文件末尾；`sort` 加入 import）：

```go
// SkillListItem 是 List 的一项:目录 slug 加 SKILL.md frontmatter 元数据。
type SkillListItem struct {
	Slug        string
	Name        string
	Description string
}

// List 枚举根目录下所有带合法 SKILL.md 的技能,按 slug 升序。单个技能的
// frontmatter 损坏只跳过该项,不让整个列表失败;根目录不存在返回 nil。
func (m *Manager) List() []SkillListItem {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil
	}
	var out []SkillListItem
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slug := e.Name()
		content, ok := m.Read(slug)
		if !ok {
			continue
		}
		fm, _, err := parseFrontmatter(content)
		if err != nil {
			continue
		}
		out = append(out, SkillListItem{Slug: slug, Name: fm.Name, Description: fm.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out
}
```

- [ ] **Step 4: 运行确认通过（含既有测试防回归）**

Run: `go test ./internal/skills/ -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/skills/manager.go internal/skills/manager_list_test.go
git commit -m "feat(skills): Manager.List 枚举技能并提炼 parseFrontmatter

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: skill_manage 动作核心（tools 包，不含 Registry）

**Files:**
- Create: `internal/agent/tools/skill_manage.go`
- Test: `internal/agent/tools/skill_manage_test.go`（新建）

- [ ] **Step 1: 写失败测试**

新建 `internal/agent/tools/skill_manage_test.go`：

```go
package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/skills"
)

const validSkillMD = "---\nname: Test Skill\ndescription: a test skill\n---\n1. step one\n"

type fakeSkillManageLedger struct {
	upserts []struct {
		AgentID, Slug string
		FirstCreate   bool
	}
	deletes []string
}

func (f *fakeSkillManageLedger) UpsertSkillUsage(ctx context.Context, agentID, slug, contentHash string, firstCreate bool) error {
	f.upserts = append(f.upserts, struct {
		AgentID, Slug string
		FirstCreate   bool
	}{agentID, slug, firstCreate})
	return nil
}

func (f *fakeSkillManageLedger) DeleteSkillUsage(ctx context.Context, agentID, slug string) error {
	f.deletes = append(f.deletes, slug)
	return nil
}

func newSkillManageExec(t *testing.T, allowDelete bool) (ToolFunc, *skills.Manager, *fakeSkillManageLedger) {
	t.Helper()
	mgr := skills.NewManager(filepath.Join(t.TempDir(), "skills"), skills.DefaultManagerConfig())
	ledger := &fakeSkillManageLedger{}
	fn := SkillManageExec(SkillManageDeps{
		Manager:     mgr,
		Upserter:    ledger,
		Deleter:     ledger,
		AgentID:     "agent-1",
		AllowDelete: allowDelete,
	})
	return fn, mgr, ledger
}

func execSkillManage(t *testing.T, fn ToolFunc, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return fn(context.Background(), raw)
}

func TestSkillManageCreateReadList(t *testing.T) {
	fn, mgr, ledger := newSkillManageExec(t, true)

	out, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "test-skill", "content": validSkillMD})
	if err != nil || !strings.Contains(out, "test-skill") {
		t.Fatalf("create = (%q, %v), want success mentioning slug", out, err)
	}
	if _, ok := mgr.Read("test-skill"); !ok {
		t.Fatal("skill not on disk after create")
	}
	if len(ledger.upserts) != 1 || !ledger.upserts[0].FirstCreate || ledger.upserts[0].AgentID != "agent-1" {
		t.Fatalf("ledger upserts = %+v, want one firstCreate for agent-1", ledger.upserts)
	}

	got, err := execSkillManage(t, fn, map[string]any{"action": "read", "slug": "test-skill"})
	if err != nil || got != validSkillMD {
		t.Fatalf("read = (%q, %v), want stored content", got, err)
	}

	listOut, err := execSkillManage(t, fn, map[string]any{"action": "list"})
	if err != nil || !strings.Contains(listOut, "test-skill") || !strings.Contains(listOut, "a test skill") {
		t.Fatalf("list = (%q, %v), want slug and description", listOut, err)
	}
}

func TestSkillManageUpdateAndDeleteSyncLedger(t *testing.T) {
	fn, mgr, ledger := newSkillManageExec(t, true)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "s", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(validSkillMD, "step one", "step two", 1)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "update", "slug": "s", "content": updated}); err != nil {
		t.Fatal(err)
	}
	if got, _ := mgr.Read("s"); !strings.Contains(got, "step two") {
		t.Fatalf("content after update = %q", got)
	}
	if len(ledger.upserts) != 2 || ledger.upserts[1].FirstCreate {
		t.Fatalf("ledger upserts = %+v, want second with firstCreate=false", ledger.upserts)
	}

	if _, err := execSkillManage(t, fn, map[string]any{"action": "delete", "slug": "s"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mgr.Read("s"); ok {
		t.Fatal("skill still on disk after delete")
	}
	if len(ledger.deletes) != 1 || ledger.deletes[0] != "s" {
		t.Fatalf("ledger deletes = %v, want [s]", ledger.deletes)
	}
}

func TestSkillManageErrorsSurfaceToCaller(t *testing.T) {
	fn, _, _ := newSkillManageExec(t, true)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "dup", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	// 重复 create → manager 拒绝,错误必须返回给模型而非静默丢弃
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "dup", "content": validSkillMD}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate create err = %v, want already-exists", err)
	}
	// 缺 frontmatter → 校验错误上抛
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "bad", "content": "no frontmatter"}); err == nil {
		t.Fatal("invalid content create succeeded, want validation error")
	}
	// 不存在的 update
	if _, err := execSkillManage(t, fn, map[string]any{"action": "update", "slug": "ghost", "content": validSkillMD}); err == nil {
		t.Fatal("update of missing skill succeeded")
	}
	// 未知 action
	if _, err := execSkillManage(t, fn, map[string]any{"action": "explode"}); err == nil {
		t.Fatal("unknown action succeeded")
	}
}

func TestSkillManageDeleteDisallowed(t *testing.T) {
	fn, mgr, _ := newSkillManageExec(t, false)
	if _, err := execSkillManage(t, fn, map[string]any{"action": "create", "slug": "keep", "content": validSkillMD}); err != nil {
		t.Fatal(err)
	}
	if _, err := execSkillManage(t, fn, map[string]any{"action": "delete", "slug": "keep"}); err == nil {
		t.Fatal("delete succeeded with AllowDelete=false")
	}
	if _, ok := mgr.Read("keep"); !ok {
		t.Fatal("skill removed despite delete being disallowed")
	}
}

func TestSkillManageNilManagerFails(t *testing.T) {
	fn := SkillManageExec(SkillManageDeps{})
	if _, err := execSkillManage(t, fn, map[string]any{"action": "list"}); err == nil {
		t.Fatal("nil manager list succeeded, want not-configured error")
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/agent/tools/ -run TestSkillManage -v`
Expected: 编译失败 `undefined: SkillManageExec` 等

- [ ] **Step 3: 最小实现**

新建 `internal/agent/tools/skill_manage.go`：

```go
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
```

- [ ] **Step 4: 运行确认通过**

Run: `go test ./internal/agent/tools/ -run TestSkillManage -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/tools/skill_manage.go internal/agent/tools/skill_manage_test.go
git commit -m "feat(tools): skill_manage 动作核心(create/update/read/delete/list + 账本同步)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Registry 集成（builtin 注册 + owner 写门控 + ForTurn 复制）

**Files:**
- Modify: `internal/agent/tools/registry.go`
- Modify: `internal/agent/tools/skill_manage.go`（追加 registerSkillManage）
- Test: `internal/agent/tools/skill_manage_test.go`（追加）

- [ ] **Step 1: 写失败测试**

追加到 `internal/agent/tools/skill_manage_test.go`。**注意**：Registry 的构造方式参照同包 `memory_tool_test.go` 的 `newMemoryToolTestRegistry`（用其真实的 NewRegistry 调用形态；工具执行沿用该文件取 `r.tools[name]` 处理函数的既有方式，helper 名以实际字段为准）：

```go
func newSkillManageTestRegistry(t *testing.T) *Registry {
	t.Helper()
	// 构造参照 newMemoryToolTestRegistry;仅需一个能跑 builtin 的空 Registry。
	r := newMemoryToolTestRegistryBare(t) // 若无此 helper,按 memory_tool_test.go 的 NewRegistry 调用复制一份
	r.SetAgentOwnerUserID("owner-1")
	mgr := skills.NewManager(filepath.Join(t.TempDir(), "skills"), skills.DefaultManagerConfig())
	r.SetSkillManage(mgr, nil)
	return r
}

func execRegistrySkillManage(t *testing.T, r *Registry, args map[string]any) (string, error) {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := r.tools["skill_manage"]
	if !ok {
		t.Fatal("skill_manage not registered")
	}
	return tool.fn(context.Background(), raw) // 字段名以 registeredTool 实际定义为准
}

func TestSkillManageOwnerGateOnTurnRegistry(t *testing.T) {
	base := newSkillManageTestRegistry(t)

	guest := base.ForTurn()
	guest.SetChatterUserID("guest-9")
	if _, err := execRegistrySkillManage(t, guest, map[string]any{
		"action": "create", "slug": "s1", "content": validSkillMD,
	}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("guest create err = %v, want owner-restriction refusal", err)
	}
	// read/list 不受门控
	if _, err := execRegistrySkillManage(t, guest, map[string]any{"action": "list"}); err != nil {
		t.Fatalf("guest list err = %v, want allowed", err)
	}

	owner := base.ForTurn()
	owner.SetChatterUserID("owner-1")
	if _, err := execRegistrySkillManage(t, owner, map[string]any{
		"action": "create", "slug": "s1", "content": validSkillMD,
	}); err != nil {
		t.Fatalf("owner create err = %v, want allowed", err)
	}

	// chatter 未设置(web 回合/旧版单用户)视为所有者语境
	blank := base.ForTurn()
	if _, err := execRegistrySkillManage(t, blank, map[string]any{
		"action": "update", "slug": "s1", "content": strings.Replace(validSkillMD, "step one", "step two", 1),
	}); err != nil {
		t.Fatalf("blank-chatter update err = %v, want allowed", err)
	}
}

func TestSkillManageDepsSurviveForTurn(t *testing.T) {
	base := newSkillManageTestRegistry(t)
	turn := base.ForTurn()
	turn.SetChatterUserID("owner-1")
	if _, err := execRegistrySkillManage(t, turn, map[string]any{
		"action": "create", "slug": "turn-skill", "content": validSkillMD,
	}); err != nil {
		t.Fatalf("create on ForTurn copy err = %v — skillManager/skillLedger 未复制进回合副本", err)
	}
}
```

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/agent/tools/ -run TestSkillManage -v`
Expected: 编译失败 `undefined: (*Registry).SetSkillManage`

- [ ] **Step 3: 实现 Registry 侧**

`internal/agent/tools/registry.go` 四处修改：

(a) `Registry` 结构体（`managedMemoryCfg` 字段附近）追加：

```go
	// skillManager / skillLedger 是 skill_manage 工具的依赖:learner 的
	// agent 级技能管理器与生命周期账本。经 SetSkillManage 装配;账本在
	// manager 装配 dataStore 时补齐。agent 级共享依赖,ForTurn 按值复制。
	skillManager *skills.Manager
	skillLedger  SkillManageLedger
```

（import 增加 `"github.com/qs3c/bkcrab/internal/skills"`。）

(b) `ForTurn` 的共享依赖复制块（`managedMemoryCfg: r.managedMemoryCfg,` 之后）追加：

```go
		skillManager:     r.skillManager,
		skillLedger:      r.skillLedger,
```

(c) Setter（放在 `SetAgentOwnerUserID` 之后）：

```go
// SetSkillManage 装配 skill_manage 工具的依赖:learner 的技能管理器与生命
// 周期账本(可为 nil,无 store 时跳过记账)。builtin 处理器按调用时读取字段,
// 因此 manager 装配路径晚于构造注册也无需重新注册。
func (r *Registry) SetSkillManage(mgr *skills.Manager, ledger SkillManageLedger) {
	r.skillManager = mgr
	r.skillLedger = ledger
}

// skillWriteAllowed 判定本回合聊天者是否可写 agent 级共享技能库。所有者键
// 取 agentOwnerUserID(agent.user_id),单用户安装未设置时回退 UserSpace 所有
// 者 userID;chatterUserID 为空表示本回合就是所有者语境(web 回合或旧版单用
// 户),仅 IM 多发件人回合的非所有者聊天者被拒。
func (r *Registry) skillWriteAllowed() bool {
	owner := r.agentOwnerUserID
	if owner == "" {
		owner = r.userID
	}
	return owner == "" || r.chatterUserID == "" || r.chatterUserID == owner
}
```

(d) `registerBuiltins`（`registerMemory(r)` 之后）追加 `registerSkillManage(r)`。

在 `internal/agent/tools/skill_manage.go` 追加注册函数：

```go
// registerSkillManage 把 skill_manage 注册为 builtin。依赖按调用时从
// Registry 字段读取;写动作过 owner 门控。ForTurn 重新注册 builtins,
// 闭包因此捕获回合副本、读到本回合的 chatterUserID。
func registerSkillManage(r *Registry) {
	r.Register("skill_manage", skillManageDescription, skillManageSchema(), makeSkillManage(
		func() SkillManageDeps {
			return SkillManageDeps{
				Manager:     r.skillManager,
				Upserter:    r.skillLedger,
				Deleter:     r.skillLedger,
				AgentID:     r.agentID,
				AllowDelete: true,
			}
		},
		func(action string) error {
			if r.skillWriteAllowed() {
				return nil
			}
			return fmt.Errorf("skill %s is restricted to the agent owner; suggest the change to the owner instead", action)
		},
	))
}
```

**注意**：`Upserter: r.skillLedger` 把超集接口赋给子集接口字段——`r.skillLedger` 为 nil 接口时结果仍是 nil，`upsertSkillLedger` 的 nil 判定安全。

- [ ] **Step 4: 运行确认通过（全包防回归）**

Run: `go test ./internal/agent/tools/ -v`
Expected: 全部 PASS（含既有 memory/file 等测试）

- [ ] **Step 5: Commit**

```bash
git add internal/agent/tools/registry.go internal/agent/tools/skill_manage.go internal/agent/tools/skill_manage_test.go
git commit -m "feat(tools): skill_manage 注册为 builtin,写动作仅 agent 所有者

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: 提取路径改有界工具循环（删除 JSON 裸解析）

**Files:**
- Modify: `internal/agent/skills_learner.go`
- Test: `internal/agent/skills_learner_test.go`（重写提取相关测试）

- [ ] **Step 1: 重写测试（失败态）**

`internal/agent/skills_learner_test.go` 修改：

(a) `learnerFakeProvider` 改为脚本化 `*provider.Response`（替换原 string 切片版本；保留 ChatStream stub）：

```go
type learnerFakeProvider struct {
	responses []*provider.Response
	calls     int
	err       error
	// prompts 记录每次调用的最后一条消息内容(素材或工具结果),msgs 记录全量。
	prompts  []string
	msgs     [][]provider.Message
	toolDefs [][]provider.Tool
}

func (p *learnerFakeProvider) Chat(ctx context.Context, messages []provider.Message, tools []provider.Tool, model string, maxTokens int, temperature float64) (*provider.Response, error) {
	if p.err != nil {
		return nil, p.err
	}
	if len(messages) > 0 {
		p.prompts = append(p.prompts, messages[len(messages)-1].Content)
	}
	p.msgs = append(p.msgs, append([]provider.Message(nil), messages...))
	p.toolDefs = append(p.toolDefs, tools)
	if p.calls >= len(p.responses) {
		p.calls++
		return &provider.Response{Content: "Nothing to save."}, nil
	}
	resp := p.responses[p.calls]
	p.calls++
	return resp, nil
}
```

(b) 工具调用响应构造 helper（替换 `learnerExtractionJSON` / `learnerUpdateJSON`，两者删除）：

```go
func skillToolCallResp(t *testing.T, id string, args map[string]any) *provider.Response {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return &provider.Response{ToolCalls: []provider.ToolCall{{
		ID:       id,
		Type:     "function",
		Function: provider.FunctionCall{Name: "skill_manage", Arguments: string(raw)},
	}}}
}

func textResp(s string) *provider.Response { return &provider.Response{Content: s} }
```

(c) 核心用例（替换原 JSON 断言类用例；`fakeSkillLedger`、`writeExistingSkill`、`readSkill`、阈值/空素材用例保留，素材完整性断言改用 `p.prompts[0]`）：

```go
func TestExtractFromSessionCreatesViaToolCall(t *testing.T) {
	ws := t.TempDir()
	p := &learnerFakeProvider{responses: []*provider.Response{
		skillToolCallResp(t, "tc1", map[string]any{"action": "create", "slug": "test-skill", "content": learnerValidSkill}),
		textResp("done"),
	}}
	sl := NewSkillsLearner(ws, p, "m")
	ledger := &fakeSkillLedger{}
	sl.ledger, sl.agentID = ledger, "agent-1"

	if err := sl.ExtractFromSession(context.Background(), []store.SessionMessage{{Role: "user", Content: "do the thing"}}); err != nil {
		t.Fatal(err)
	}
	if got, ok := readSkill(t, ws, "test-skill"); !ok || got != learnerValidSkill {
		t.Fatalf("skill on disk = (%q, %v), want created content", got, ok)
	}
	if len(ledger.calls) != 1 || !ledger.calls[0].FirstCreate {
		t.Fatalf("ledger calls = %+v, want one firstCreate", ledger.calls)
	}
	if p.calls != 2 {
		t.Fatalf("provider calls = %d, want 2 (act + finish)", p.calls)
	}
	if len(p.toolDefs[0]) != 1 || p.toolDefs[0][0].Function.Name != "skill_manage" {
		t.Fatalf("toolDefs[0] = %+v, want only skill_manage", p.toolDefs[0])
	}
}

func TestExtractFromSessionNothingToSave(t *testing.T) {
	ws := t.TempDir()
	p := &learnerFakeProvider{responses: []*provider.Response{textResp("Nothing to save.")}}
	sl := NewSkillsLearner(ws, p, "m")
	if err := sl.ExtractFromSession(context.Background(), []store.SessionMessage{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatal(err)
	}
	if p.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", p.calls)
	}
	if entries, _ := os.ReadDir(filepath.Join(ws, "skills")); len(entries) != 0 {
		t.Fatalf("skills dir entries = %d, want 0", len(entries))
	}
}

func TestExtractFromSessionProviderErrorPropagates(t *testing.T) {
	p := &learnerFakeProvider{err: errors.New("boom")}
	sl := NewSkillsLearner(t.TempDir(), p, "m")
	if err := sl.ExtractFromSession(context.Background(), []store.SessionMessage{{Role: "user", Content: "x"}}); err == nil {
		t.Fatal("want infra error to propagate (caller resets batch)")
	}
}

func TestExtractFromSessionMergeReadsThenUpdates(t *testing.T) {
	ws := t.TempDir()
	existing := strings.Replace(learnerValidSkill, "second step", "old step", 1)
	writeExistingSkill(t, ws, "test-skill", existing)
	merged := strings.Replace(learnerValidSkill, "second step", "merged step", 1)
	p := &learnerFakeProvider{responses: []*provider.Response{
		skillToolCallResp(t, "tc1", map[string]any{"action": "read", "slug": "test-skill"}),
		skillToolCallResp(t, "tc2", map[string]any{"action": "update", "slug": "test-skill", "content": merged}),
		textResp("done"),
	}}
	sl := NewSkillsLearner(ws, p, "m")
	ledger := &fakeSkillLedger{}
	sl.ledger, sl.agentID = ledger, "agent-1"

	if err := sl.ExtractFromSession(context.Background(), []store.SessionMessage{{Role: "user", Content: "again"}}); err != nil {
		t.Fatal(err)
	}
	// read 的工具结果必须把现有技能全文带回给模型
	if p.prompts[1] != existing {
		t.Fatalf("tool result fed back = %q, want existing skill content", p.prompts[1])
	}
	if got, _ := readSkill(t, ws, "test-skill"); !strings.Contains(got, "merged step") {
		t.Fatalf("skill after merge = %q", got)
	}
	if len(ledger.calls) != 1 || ledger.calls[0].FirstCreate {
		t.Fatalf("ledger calls = %+v, want one update(firstCreate=false)", ledger.calls)
	}
}

func TestExtractFromSessionValidationErrorFedBack(t *testing.T) {
	ws := t.TempDir()
	p := &learnerFakeProvider{responses: []*provider.Response{
		skillToolCallResp(t, "tc1", map[string]any{"action": "create", "slug": "test-skill", "content": "no frontmatter"}),
		skillToolCallResp(t, "tc2", map[string]any{"action": "create", "slug": "test-skill", "content": learnerValidSkill}),
		textResp("done"),
	}}
	sl := NewSkillsLearner(ws, p, "m")
	if err := sl.ExtractFromSession(context.Background(), []store.SessionMessage{{Role: "user", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	// 第一次 create 的校验错误作为工具结果反馈,模型第二次修正成功
	if !strings.Contains(p.prompts[1], "frontmatter") {
		t.Fatalf("fed-back error = %q, want frontmatter validation message", p.prompts[1])
	}
	if _, ok := readSkill(t, ws, "test-skill"); !ok {
		t.Fatal("corrected create did not land")
	}
}

func TestExtractFromSessionDeleteRefused(t *testing.T) {
	ws := t.TempDir()
	writeExistingSkill(t, ws, "keep-me", learnerValidSkill)
	p := &learnerFakeProvider{responses: []*provider.Response{
		skillToolCallResp(t, "tc1", map[string]any{"action": "delete", "slug": "keep-me"}),
		textResp("ok"),
	}}
	sl := NewSkillsLearner(ws, p, "m")
	if err := sl.ExtractFromSession(context.Background(), []store.SessionMessage{{Role: "user", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSkill(t, ws, "keep-me"); !ok {
		t.Fatal("extraction loop deleted a skill — delete must be disabled")
	}
}

func TestExtractFromSessionIterationCap(t *testing.T) {
	ws := t.TempDir()
	loop := skillToolCallResp(t, "tc", map[string]any{"action": "read", "slug": "nope"})
	p := &learnerFakeProvider{responses: []*provider.Response{loop, loop, loop, loop, loop, loop}}
	sl := NewSkillsLearner(ws, p, "m")
	if err := sl.ExtractFromSession(context.Background(), []store.SessionMessage{{Role: "user", Content: "x"}}); err != nil {
		t.Fatal(err)
	}
	if p.calls != skillExtractMaxIterations {
		t.Fatalf("provider calls = %d, want cap %d", p.calls, skillExtractMaxIterations)
	}
}
```

同时**改造既有用例**：凡构造 `learnerFakeProvider{responses: []string{...}}` 的地方（阈值用例、空素材用例、素材完整性用例等）改为 `[]*provider.Response` 形态；素材完整性断言不变（`p.prompts[0]` 仍是第一次调用的 user 素材）。删除 `learnerExtractionJSON` / `learnerUpdateJSON` 及仅服务于 JSON 路径的用例（如"decideUpdate 不更新"类）。

- [ ] **Step 2: 运行确认失败**

Run: `go test ./internal/agent/ -run 'TestExtractFromSession|TestMaybeExtract' -v`
Expected: 编译失败 `undefined: skillExtractMaxIterations` 等

- [ ] **Step 3: 重写 skills_learner.go 提取核心**

(a) **删除**以下符号及其引用：`extractedSkill`、`extractionResponse`、`extractFromMaterial`、`persistExtracted`、`upsertLedger`、`decideUpdate`、`updateDecision`、`updateDecisionPrompt`、agent 包本地接口 `skillLedger`。

(b) `SkillsLearner.ledger` 字段类型改为 `tools.SkillLedgerUpserter`（import `"github.com/qs3c/bkcrab/internal/agent/tools"`；manager.go 的 `ag.skillsLearner.ledger = m.opts.dataStore` 赋值兼容，无需改动）。

(c) 新增循环常量与核心：

```go
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
				slog.Info("skill extraction action applied", "agent", sl.agentID, "result", out)
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
```

(d) 两个入口改为薄封装（注释保留问题一的素材说明）：

```go
func (sl *SkillsLearner) MaybeExtract(ctx context.Context, messages []provider.Message, toolCallCount int) error {
	if toolCallCount < sl.minToolCalls {
		return nil
	}
	return sl.extractWithTools(ctx, renderProviderMessages(messages))
}

func (sl *SkillsLearner) ExtractFromSession(ctx context.Context, msgs []store.SessionMessage) error {
	material := renderSessionMessages(msgs)
	if strings.TrimSpace(material) == "" {
		return nil
	}
	return sl.extractWithTools(ctx, material)
}
```

（原有的 ExtractFromSession 文档注释保留在函数上方。）

(e) `fallbackExtractionPrompt` 整体替换为工具驱动版：

```go
const fallbackExtractionPrompt = `You maintain this agent's skill library. Analyze the conversation and decide whether it demonstrates a reusable multi-step skill worth saving, acting through the skill_manage tool.

The input is the full working context of one session: recent messages verbatim; if the session was long, the older span appears as a [Conversation Summary] block.

Save a skill when the conversation shows at least one of:
- A repeatable multi-step workflow: multiple tool calls in a clear sequence, general enough to be useful in other contexts
- A hard-won approach: the task required trial and error, or the course changed because of findings along the way — capture the path that worked and the dead ends to avoid
- An expectation correction: the user expected a different method or outcome than the first attempt

How to act:
- New skill: call skill_manage {action:"create", slug:"kebab-case-slug", content:"..."}. content is a full SKILL.md: YAML frontmatter with non-empty name and description, then step-by-step markdown instructions.
- A listed existing skill covers the same workflow: first call {action:"read", slug} to see its current content, then {action:"update", slug, content} with a merged version keeping the best of both. Skip the update if the existing skill already covers everything this conversation taught.
- If a call is rejected, fix the content per the error message and retry once.
- Nothing worth saving: do not call any tool; reply with the single line: Nothing to save.`
```

- [ ] **Step 4: 运行确认通过（agent 包全量防回归）**

Run: `go test ./internal/agent/ -v -count=1`
Expected: 全部 PASS（特别关注 skills_cadence / loop_usage 等间接引用者编译通过）

- [ ] **Step 5: Commit**

```bash
git add internal/agent/skills_learner.go internal/agent/skills_learner_test.go
git commit -m "refactor(agent): 技能提取改有界 skill_manage 工具循环,删除 JSON 裸解析

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: 生产接线 + 提示词文件重写

**Files:**
- Modify: `internal/agent/loop.go`（约 :260，NewSkillsLearner 之后）
- Modify: `internal/agent/manager.go`（约 :250-251，learner ledger 补丁处）
- Modify: `skills/bkcrab-skill-learner/SKILL.md`

- [ ] **Step 1: loop.go 构造器接线**

在 `ag.skillsLearner.agentID = rc.ID`（loop.go:261）之后追加（确认此处 `ag.registry` 已构造，若在其前则移到 registry 构造之后）：

```go
	// skill_manage 依赖装配:构造期先接管理器(账本此时未装配,manager
	// 装配 dataStore 时经 SetSkillManage 补齐)。
	ag.registry.SetSkillManage(ag.skillsLearner.Manager(), nil)
```

- [ ] **Step 2: manager.go 生产装配接线**

在 `ag.skillsLearner.ledger = m.opts.dataStore`（manager.go:251）同一保护块内追加（沿用该块对 dataStore 的既有判空守卫）：

```go
			ag.registry.SetSkillManage(ag.skillsLearner.Manager(), m.opts.dataStore)
```

**验证生产路径**：确认该块位于 buildAgent/NewAgentWithSkillsCfg 装配链上（生产 agent 构造只走这条链——只接其他构造器会生产失效）。

- [ ] **Step 3: 重写 skills/bkcrab-skill-learner/SKILL.md**

frontmatter 与"When to Extract"、"How to Analyze"、"Skill Content Guidelines"、"Example Extraction"各节保留；`## Input` 保留；**删除 `## Output Format` 一节**，替换为：

```markdown
## How to Act

You have ONE tool: `skill_manage`. Act through it — do not output JSON in text.

- **New skill**: call `skill_manage` with `{action:"create", slug:"kebab-case-slug", content:"..."}`. `content` is a full SKILL.md: YAML frontmatter with non-empty `name` and `description`, then step-by-step markdown instructions.
- **A listed existing skill covers the same workflow**: first call `{action:"read", slug}` to see its current content, then call `{action:"update", slug, content}` with a merged version that keeps the best of both and adds what this conversation taught. If the existing skill already covers everything, stop without updating.
- **A call is rejected** (validation or safety scan): fix the content per the error message and retry once.
- **Nothing worth saving**: do not call any tool; reply with the single line `Nothing to save.`

You have a small iteration budget (about 4 rounds). Be decisive: read at most one existing skill, then create or update in the next call.
```

同时删除原文第 31 行"If a skill with the same slug already exists, still output the extraction. The runtime will compare both versions and decide whether to merge them."（合并决策已移交模型自身的 read→update 流程）。

- [ ] **Step 4: 编译 + 相关测试**

Run: `go build ./... ; go test ./internal/agent/ ./internal/agent/tools/ -count=1`
Expected: 编译通过，全部 PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/loop.go internal/agent/manager.go skills/bkcrab-skill-learner/SKILL.md
git commit -m "feat(agent): skill_manage 生产接线(构造器+manager 装配)与提示词工具化

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: 死代码清扫 + 全量验证

**Files:**
- 全仓验证，无预期新改动

- [ ] **Step 1: 死引用清扫**

Run: `grep -rn "decideUpdate\|updateDecisionPrompt\|extractFromMaterial\|persistExtracted\|extractionResponse\|extractedSkill\|learnerExtractionJSON\|learnerUpdateJSON" internal/ --include="*.go"`
Expected: 无输出。有输出则逐个清理（多为测试残留）。

- [ ] **Step 2: 全量构建 + 测试**

Run: `go build ./... ; go vet ./... ; go test ./... -count=1`
Expected: 全部通过。失败则修复后重跑，不得跳过。

- [ ] **Step 3: Commit（如有清扫改动）**

```bash
git add -A
git commit -m "chore(agent): skill_manage 工具化收尾清扫

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

## 自查记录

- **规格覆盖**：工具五动作（T2）、主对话注册（T3 registerBuiltins）、owner 写门控（T3）、提取工具循环 ≤4 轮/禁 delete/错误反馈（T4）、两处 JSON 解析删除（T4）、账本同步 create/update/delete（T2）、零 DB 变更（无迁移任务）、生产路径接线（T5 含 buildAgent 链验证）——全覆盖。
- **类型一致性**：`SkillManageDeps{Manager, Upserter, Deleter, AgentID, AllowDelete}` 在 T2 定义、T3/T4 使用一致；`provider.Response`（非 ChatResponse）与 `FunctionCall{Name, Arguments}` 与仓库实际类型一致；`sl.ledger` 类型改 `tools.SkillLedgerUpserter` 后 manager.go 赋值兼容已确认。
- **已知执行期核对点**（非占位符，是仓库事实核对）：T3 测试中 Registry 构造 helper 与 `registeredTool` 的函数字段名以 `memory_tool_test.go` 实际写法为准；T5 loop.go 追加位置需确认 `ag.registry` 构造顺序；manager.go 追加沿用既有判空守卫。
