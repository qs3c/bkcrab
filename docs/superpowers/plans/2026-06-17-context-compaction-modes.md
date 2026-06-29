# 上下文压缩模式实现计划

> **给 agentic workers 的要求:** 执行本计划时必须使用 `superpowers:subagent-driven-development`，推荐；或使用 `superpowers:executing-plans`。步骤使用复选框格式，便于逐项推进和复核。

**目标:** 为 BkCrab 增加三类上下文压缩流程：预判性压缩、手动 `/compact` 检查点压缩、上下文超限后的紧急压缩；同时移除对 `memory_log` 的 tool result 占位依赖，并在 LLM 总结失败时降级到非 LLM 压缩。

**架构:** 现有 `CompactMessages` 从单一路径改为 options-driven 的压缩引擎。模型上下文窗口从 provider model metadata 解析，agent 在调用压缩时传入模型预算、输出预留、系统提示和工具定义等请求开销。旧 tool result 使用本地硬规则生成一行摘要，LLM summary 失败会重试 3 次，然后使用 deterministic fallback。

**技术栈:** Go，现有 `provider.Provider` 接口，现有 `session.Manager.ReplaceMessages`，现有 slash command 和 ReAct loop。

---

## 基线状态

工作区：

- 路径：`D:\fromGithub\bkcrab\.worktrees\context-compaction-modes`
- 分支：`context-compaction-modes`
- 基于：`main`
- 已确认是 linked worktree：`git rev-parse --git-dir` 与 `git rev-parse --git-common-dir` 不同。

基线测试：

- 沙箱内 `go test ./...` 会被 Go build cache 权限挡住。
- 非沙箱执行 `go test ./...` 在 main 上已有失败：
  - `cmd/bkcrab` 和 `internal/setup`：`internal\setup\embed.go:5:12: pattern all:web: no matching files found`
  - `internal/agent/tools`：`TestIdentityFileBlockedRespectsCallerFlag` 对 `/var/lib/bkcrab/agents/xyz/SOUL.md` 的阻断断言失败。
- 与本次改动最相关的 `internal/agent` 基线已通过。

## 测试环境硬约束

本机安全软件会拦截带 `claw` 字样的 exe，也会拦截路径中带 `claw` 的可执行文件运行。执行本计划时遵守以下规则：

- 不从 `D:\fromGithub\bkcrab` 或其子路径运行任何生成的 exe。
- 不主动生成文件名含 `claw` 的 exe。
- 所有 Go 测试都把 build cache 和临时目录指到不含 `claw` 的路径。
- 避免执行 `go test ./...`，因为它可能编译或运行 `cmd/bkcrab` 相关 test binary。

每次执行 Go 测试前，在同一个 PowerShell 会话中设置：

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
```

如果确实需要手工 build 二进制，只能输出到中性路径和中性文件名：

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-bin | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go build -o D:\tmp\ctxmode-bin\agent.exe ./cmd/bkcrab
```

本计划默认只跑聚焦包测试：`./internal/config`、`./internal/gateway`、`./internal/agent`。

## 文件结构

- 修改 `internal/config/config.go`
  - 增加 `ResolvedAgent.ContextWindow`。
  - 增加 `DefaultContextWindow`、`ResolveContextWindow`、`(*ResolvedAgent).RefreshModelContextWindow`。
  - 在 `MergedAgentConfig` 返回前刷新上下文窗口。

- 新增 `internal/config/context_window_test.go`
  - 覆盖 provider 前缀模型、无前缀模型、模型显示名和 fallback。

- 修改 `internal/gateway/userspace.go`
  - agent scoped provider/model/max token 覆盖后重新计算 `ContextWindow`。

- 修改 `internal/agent/loop.go`
  - `Agent` 增加 `contextWindow` 和 provider config metadata。
  - 预判性压缩时传入 request overhead 和 tool definitions。
  - LLM 返回 context limit error 时触发紧急压缩、替换 session messages、重建 request 并重试一次。

- 修改 `internal/agent/slash.go`
  - `/compact` 参数作为 manual focus。
  - `/compact` 使用强制 manual checkpoint，不受预判阈值限制。
  - `/model` 切换后刷新 `a.contextWindow`。

- 修改 `internal/agent/compaction.go`
  - 保留兼容 wrapper，但内部走 `CompactMessagesWithOptions`。
  - 增加压缩模式、预算计算、tail turn 选择、LLM summary retry/fallback、emergency compression。

- 新增 `internal/agent/compaction_tool.go`
  - 本地 deterministic tool result 摘要规则。
  - 旧 assistant tool-call arguments 截断，尽量保持 JSON 有效。
  - tool pair sanitizer，删除 orphan tool result 和缺失结果的 assistant tool-call group。

- 新增或修改测试：
  - `internal/agent/compaction_budget_test.go`
  - `internal/agent/compaction_test.go`
  - `internal/agent/compaction_tool_test.go`
  - `internal/agent/compaction_fallback_test.go`

---

### Task 1：模型上下文窗口解析

**文件：**
- 修改：`internal/config/config.go`
- 修改：`internal/gateway/userspace.go`
- 新增测试：`internal/config/context_window_test.go`

- [ ] **Step 1：先写失败测试**

创建 `internal/config/context_window_test.go`：

```go
package config

import "testing"

func TestResolveContextWindowUsesProviderPrefixedModelID(t *testing.T) {
	providers := map[string]ProviderConfig{
		"openai": {
			Models: []ModelEntry{
				{ID: "gpt-4.1", ContextWindow: 1048576, MaxTokens: 32768},
			},
		},
	}

	got := ResolveContextWindow(providers, "openai/gpt-4.1", 8192)
	if got != 1048576 {
		t.Fatalf("context window = %d, want 1048576", got)
	}
}

func TestResolveContextWindowUsesModelNameWhenIDDiffers(t *testing.T) {
	providers := map[string]ProviderConfig{
		"anthropic": {
			Models: []ModelEntry{
				{ID: "claude-sonnet-4", Name: "Claude Sonnet 4", ContextWindow: 200000},
			},
		},
	}

	got := ResolveContextWindow(providers, "Claude Sonnet 4", 8192)
	if got != 200000 {
		t.Fatalf("context window = %d, want 200000", got)
	}
}

func TestResolveContextWindowFallsBackToDefault(t *testing.T) {
	got := ResolveContextWindow(nil, "unknown/model", 8192)
	if got != DefaultContextWindow {
		t.Fatalf("context window = %d, want %d", got, DefaultContextWindow)
	}
}

func TestResolvedAgentRefreshModelContextWindow(t *testing.T) {
	rc := ResolvedAgent{
		Model:     "openrouter/qwen/qwen3-coder",
		MaxTokens: 12000,
		Providers: map[string]ProviderConfig{
			"openrouter": {
				Models: []ModelEntry{
					{ID: "qwen/qwen3-coder", ContextWindow: 262144},
				},
			},
		},
	}

	rc.RefreshModelContextWindow()
	if rc.ContextWindow != 262144 {
		t.Fatalf("context window = %d, want 262144", rc.ContextWindow)
	}
}
```

- [ ] **Step 2：运行测试确认失败**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/config
```

预期：失败，因为相关类型和函数尚未存在。

- [ ] **Step 3：实现 config 侧解析**

在 `internal/config/config.go` 中增加：

```go
const DefaultContextWindow = 128000

func ResolveContextWindow(providers map[string]ProviderConfig, model string, maxTokens int) int {
	model = strings.TrimSpace(model)
	if model == "" {
		return DefaultContextWindow
	}

	providerName, modelName := splitProviderModel(model)
	if providerName != "" {
		if pc, ok := providers[providerName]; ok {
			if window := resolveContextWindowInProvider(pc, modelName); window > 0 {
				return window
			}
		}
	}

	for _, pc := range providers {
		if window := resolveContextWindowInProvider(pc, model); window > 0 {
			return window
		}
	}
	return DefaultContextWindow
}

func (ra *ResolvedAgent) RefreshModelContextWindow() {
	ra.ContextWindow = ResolveContextWindow(ra.Providers, ra.Model, ra.MaxTokens)
}

func splitProviderModel(model string) (string, string) {
	left, right, ok := strings.Cut(model, "/")
	if !ok || left == "" || right == "" {
		return "", model
	}
	return left, right
}

func resolveContextWindowInProvider(pc ProviderConfig, model string) int {
	needle := strings.ToLower(strings.TrimSpace(model))
	for _, entry := range pc.Models {
		if entry.ContextWindow <= 0 {
			continue
		}
		if strings.ToLower(entry.ID) == needle || strings.ToLower(entry.Name) == needle {
			return entry.ContextWindow
		}
	}
	return 0
}
```

给 `ResolvedAgent` 增加字段：

```go
ContextWindow int
```

在 `MergedAgentConfig` 返回前调用：

```go
resolved.RefreshModelContextWindow()
return resolved
```

- [ ] **Step 4：userspace 覆盖后刷新**

在 `internal/gateway/userspace.go` 中，agent scoped provider/model/max token 覆盖完成后、`ensureAgentHome(*rc)` 前加入：

```go
rc.RefreshModelContextWindow()
```

`EnsureAgent` 中若存在直接修改 `rc.Model` 或 `rc.MaxTokens` 的路径，也在最终使用前调用同样逻辑。

- [ ] **Step 5：运行聚焦测试**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/config ./internal/gateway
```

预期：通过。

- [ ] **Step 6：提交**

```powershell
git add internal/config/config.go internal/config/context_window_test.go internal/gateway/userspace.go
git commit -m "feat: resolve model context windows"
```

---

### Task 2：压缩模式与百分比预算

**文件：**
- 修改：`internal/agent/compaction.go`
- 新增测试：`internal/agent/compaction_budget_test.go`
- 修改测试：`internal/agent/compaction_test.go`

- [ ] **Step 1：先写预算测试**

创建 `internal/agent/compaction_budget_test.go`：

```go
package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
)

type countingSummarizer struct {
	calls int
}

func (f *countingSummarizer) Chat(_ context.Context, _ []provider.Message, _ []provider.Tool, _ string, _ int, _ float64) (*provider.Response, error) {
	f.calls++
	return &provider.Response{Content: "summary"}, nil
}

func (f *countingSummarizer) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, nil
}

func TestProactiveCompactionUsesPercentOfContextWindow(t *testing.T) {
	msgs := make([]provider.Message, 0, 40)
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: strings.Repeat("u", 200)},
			provider.Message{Role: "assistant", Content: strings.Repeat("a", 200)},
		)
	}

	f := &countingSummarizer{}
	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:            CompactModeProactive,
		Workspace:       t.TempDir(),
		Provider:        f,
		Model:           "fake-model",
		ContextWindow:   1200,
		MaxOutputTokens: 200,
		TriggerPercent: 75,
		TargetPercent:  55,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !out.Pruned {
		t.Fatal("expected proactive compaction to trigger")
	}
	if f.calls != 1 {
		t.Fatalf("summary calls = %d, want 1", f.calls)
	}
}

func TestProactiveCompactionIncludesRequestOverhead(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: strings.Repeat("x", 1200)},
		{Role: "assistant", Content: strings.Repeat("y", 1200)},
	}
	overhead := []provider.Message{
		{Role: "system", Content: strings.Repeat("s", 3000)},
	}

	f := &countingSummarizer{}
	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:             CompactModeProactive,
		Workspace:        t.TempDir(),
		Provider:         f,
		Model:            "fake-model",
		ContextWindow:    2000,
		MaxOutputTokens:  200,
		TriggerPercent:  75,
		TargetPercent:   55,
		OverheadMessages: overhead,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !out.Pruned {
		t.Fatal("expected overhead to push request over the proactive threshold")
	}
}
```

- [ ] **Step 2：运行测试确认失败**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run "TestProactiveCompaction"
```

预期：失败，因为 options API 尚未存在。

- [ ] **Step 3：新增 mode/options API**

在 `internal/agent/compaction.go` 增加：

```go
type CompactMode string

const (
	CompactModeProactive CompactMode = "proactive"
	CompactModeManual    CompactMode = "manual"
	CompactModeEmergency CompactMode = "emergency"

	DefaultCompactionTriggerPercent = 75
	DefaultCompactionTargetPercent  = 55
	DefaultTailTurns                = 4
	MinimumTailTurns                = 2
	DefaultContextWindow            = 128000
)

type CompactOptions struct {
	Mode              CompactMode
	Workspace         string
	Provider          provider.Provider
	Model             string
	ContextWindow     int
	MaxOutputTokens   int
	TriggerPercent   int
	TargetPercent    int
	TailTurns         int
	MinTailTurns      int
	Focus             string
	OverheadMessages  []provider.Message
	ToolDefs          []provider.Tool
	SummaryMaxRetries int
}
```

保留兼容 wrapper：

```go
func CompactMessages(messages []provider.Message, workspace string, prov provider.Provider, model string) (*CompactResult, error) {
	return CompactMessagesWithOptions(messages, CompactOptions{
		Mode:              CompactModeProactive,
		Workspace:         workspace,
		Provider:          prov,
		Model:             model,
		ContextWindow:     DefaultContextWindow,
		TriggerPercent:   DefaultCompactionTriggerPercent,
		TargetPercent:    DefaultCompactionTargetPercent,
		TailTurns:         DefaultTailTurns,
		MinTailTurns:      MinimumTailTurns,
		SummaryMaxRetries: 3,
	})
}
```

增加 `normalizeCompactOptions`、`EstimateRequestTokens`、`compactTriggerLimit`、`compactTargetLimit`。预算逻辑使用：

- 输入预算：`ContextWindow - MaxOutputTokens`
- 触发阈值：输入预算的 `TriggerPercent`
- 目标阈值：输入预算的 `TargetPercent`
- 估算时包含 `OverheadMessages` 和 `ToolDefs`

- [ ] **Step 4：实现 `CompactMessagesWithOptions` 分流**

核心逻辑：

```go
func CompactMessagesWithOptions(messages []provider.Message, opts CompactOptions) (*CompactResult, error) {
	opts = normalizeCompactOptions(opts)
	request := append([]provider.Message{}, opts.OverheadMessages...)
	request = append(request, messages...)
	tokens := EstimateRequestTokens(request, opts.ToolDefs)

	switch opts.Mode {
	case CompactModeManual:
		return compactWithSummary(messages, opts, tokens)
	case CompactModeEmergency:
		return emergencyCompactMessages(messages, opts, tokens), nil
	default:
		if tokens < compactTriggerLimit(opts) {
			return &CompactResult{Messages: sanitizeToolPairs(messages)}, nil
		}
		return compactWithSummary(messages, opts, tokens)
	}
}
```

- [ ] **Step 5：运行测试**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run "TestProactiveCompaction"
```

预期：通过。

- [ ] **Step 6：提交**

```powershell
git add internal/agent/compaction.go internal/agent/compaction_budget_test.go internal/agent/compaction_test.go
git commit -m "feat: add compaction modes and budgets"
```

---

### Task 3：最近上下文保留与 tool pair 清洗

**文件：**
- 修改：`internal/agent/compaction.go`
- 新增：`internal/agent/compaction_tool.go`
- 修改测试：`internal/agent/compaction_test.go`
- 新增测试：`internal/agent/compaction_tool_test.go`

- [ ] **Step 1：先写 sanitizer 和 tail policy 测试**

创建 `internal/agent/compaction_tool_test.go`：

```go
package agent

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
)

func TestSanitizeToolPairsDropsOrphanToolResult(t *testing.T) {
	in := []provider.Message{
		{Role: "user", Content: "hi"},
		{Role: "tool", ToolCallID: "missing", Content: "orphan"},
		{Role: "assistant", Content: "ok"},
	}

	out := sanitizeToolPairs(in)
	for _, msg := range out {
		if msg.Role == "tool" {
			t.Fatalf("orphan tool result survived: %+v", msg)
		}
	}
}

func TestSanitizeToolPairsDropsIncompleteToolCalls(t *testing.T) {
	in := []provider.Message{
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", ToolCallID: "a", Content: "only one result"},
		{Role: "user", Content: "next"},
	}

	out := sanitizeToolPairs(in)
	if len(out) != 1 || out[0].Role != "user" {
		t.Fatalf("incomplete tool-call group should be removed, got %+v", out)
	}
}

func TestCompactionKeepsRecentFourUserTurns(t *testing.T) {
	var msgs []provider.Message
	for i := 0; i < 8; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: "user turn"},
			provider.Message{Role: "assistant", Content: "assistant turn"},
		)
	}

	cutoff := compactionTailStart(msgs, CompactOptions{TailTurns: 4, MinTailTurns: 2})
	tailUsers := 0
	for _, msg := range msgs[cutoff:] {
		if msg.Role == "user" {
			tailUsers++
		}
	}
	if tailUsers != 4 {
		t.Fatalf("tail user turns = %d, want 4", tailUsers)
	}
}
```

- [ ] **Step 2：运行测试确认失败**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run "TestSanitizeToolPairs|TestCompactionKeepsRecentFourUserTurns"
```

预期：失败，因为函数尚未存在。

- [ ] **Step 3：实现 tool pair sanitizer**

创建 `internal/agent/compaction_tool.go`，实现：

```go
func sanitizeToolPairs(messages []provider.Message) []provider.Message {
	out := make([]provider.Message, 0, len(messages))
	for i := 0; i < len(messages); i++ {
		msg := messages[i]
		if msg.Role == "tool" {
			continue
		}
		if msg.Role != "assistant" || len(msg.ToolCalls) == 0 {
			out = append(out, msg)
			continue
		}

		expected := map[string]bool{}
		for _, tc := range msg.ToolCalls {
			expected[tc.ID] = false
		}

		j := i + 1
		var tools []provider.Message
		for j < len(messages) && messages[j].Role == "tool" {
			if _, ok := expected[messages[j].ToolCallID]; ok {
				expected[messages[j].ToolCallID] = true
				tools = append(tools, messages[j])
			}
			j++
		}

		complete := true
		for _, seen := range expected {
			if !seen {
				complete = false
				break
			}
		}
		if complete {
			out = append(out, msg)
			out = append(out, tools...)
		} else if msg.Content != "" || len(msg.ContentParts) > 0 || msg.Thinking != "" {
			msg.ToolCalls = nil
			msg.RawAssistant = nil
			out = append(out, msg)
		}
		i = j - 1
	}
	return out
}
```

- [ ] **Step 4：实现综合 tail policy**

在 `internal/agent/compaction.go` 增加：

```go
func compactionTailStart(messages []provider.Message, opts CompactOptions) int {
	opts = normalizeCompactOptions(opts)
	var starts []int
	for i, msg := range messages {
		if msg.Role == "user" && msg.Origin == provider.OriginUser {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return safeCompactionCutoff(messages, max(0, len(messages)-PruneTurnAge))
	}

	turns := opts.TailTurns
	if turns > len(starts) {
		turns = len(starts)
	}
	if turns < opts.MinTailTurns && len(starts) >= opts.MinTailTurns {
		turns = opts.MinTailTurns
	}
	return safeCompactionCutoff(messages, starts[len(starts)-turns])
}
```

设计取舍：

- pico 的“最近 4 个 turn”比当前“近 20 条消息”更贴合对话语义。
- 但纯 turn 数可能遇到超长 tool result，所以后续预算仍由 Task 2 的 percent threshold 控制。
- 保留 `safeCompactionCutoff`，避免 tail 以孤立 tool message 开始。

- [ ] **Step 5：更新旧测试调用**

把旧的：

```go
out, err := compressOlderMessages(msgs, f, "fake-model")
```

改为：

```go
out, err := compressOlderMessages(msgs, CompactOptions{
	Provider:          f,
	Model:             "fake-model",
	TailTurns:         DefaultTailTurns,
	MinTailTurns:      MinimumTailTurns,
	SummaryMaxRetries: 3,
})
```

- [ ] **Step 6：运行测试**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent
```

预期：通过。

- [ ] **Step 7：提交**

```powershell
git add internal/agent/compaction.go internal/agent/compaction_test.go internal/agent/compaction_tool.go internal/agent/compaction_tool_test.go
git commit -m "feat: sanitize compacted tool history"
```

---

### Task 4：本地 tool result 摘要

**文件：**
- 修改：`internal/agent/compaction_tool.go`
- 修改：`internal/agent/compaction.go`
- 修改测试：`internal/agent/compaction_tool_test.go`

- [ ] **Step 1：先写 tool summary 测试**

追加到 `internal/agent/compaction_tool_test.go`：

```go
func TestSummarizeTerminalToolResultUsesLocalRules(t *testing.T) {
	msg := provider.Message{
		Role:       "tool",
		ToolCallID: "call-1",
		Content:    "line 1\nline 2\nexit code: 1\n",
	}
	lookup := map[string]toolCallInfo{
		"call-1": {Name: "terminal", Arguments: `{"command":"go test ./internal/agent"}`},
	}

	got := summarizeToolResult(msg, lookup)
	if !strings.Contains(got.Content, "terminal") ||
		!strings.Contains(got.Content, "go test ./internal/agent") ||
		!strings.Contains(got.Content, "exit code 1") ||
		!strings.Contains(got.Content, "3 lines") {
		t.Fatalf("unexpected summary: %q", got.Content)
	}
	if strings.Contains(got.Content, "memory log") {
		t.Fatalf("summary must not mention memory logs: %q", got.Content)
	}
}

func TestSummarizeReadFileToolResultUsesPathAndSize(t *testing.T) {
	msg := provider.Message{
		Role:       "tool",
		ToolCallID: "call-1",
		Content:    strings.Repeat("x", 2048),
	}
	lookup := map[string]toolCallInfo{
		"call-1": {Name: "read_file", Arguments: `{"path":"internal/agent/loop.go"}`},
	}

	got := summarizeToolResult(msg, lookup)
	if !strings.Contains(got.Content, "read_file") ||
		!strings.Contains(got.Content, "internal/agent/loop.go") ||
		!strings.Contains(got.Content, "2048 chars") {
		t.Fatalf("unexpected summary: %q", got.Content)
	}
}
```

测试文件增加 import：

```go
import "strings"
```

- [ ] **Step 2：运行测试确认失败**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run "TestSummarize.*ToolResult"
```

预期：失败，因为摘要函数尚未存在。

- [ ] **Step 3：实现 tool call lookup 和本地摘要**

在 `internal/agent/compaction_tool.go` 中增加：

```go
type toolCallInfo struct {
	Name      string
	Arguments string
}

func buildToolCallLookup(messages []provider.Message) map[string]toolCallInfo {
	out := map[string]toolCallInfo{}
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		for _, tc := range msg.ToolCalls {
			out[tc.ID] = toolCallInfo{Name: tc.Function.Name, Arguments: tc.Function.Arguments}
		}
	}
	return out
}

func summarizeToolResult(msg provider.Message, lookup map[string]toolCallInfo) provider.Message {
	info := lookup[msg.ToolCallID]
	name := info.Name
	if name == "" {
		name = msg.Name
	}
	if name == "" {
		name = "tool"
	}

	args := parseToolArgs(info.Arguments)
	contentChars := len(msg.Content)
	lineCount := strings.Count(msg.Content, "\n")
	if msg.Content != "" && !strings.HasSuffix(msg.Content, "\n") {
		lineCount++
	}

	msg.Content = formatToolSummary(name, args, msg.Content, contentChars, lineCount)
	msg.Metadata = nil
	return msg
}
```

摘要规则：

- terminal/shell/command：保留 command、exit code、输出行数、字符数。
- read/read_file：保留 path 和输出字符数。
- search/grep：保留 query/pattern、path、命中输出规模。
- web search/fetch：保留 query/url 和输出规模。
- generic：保留前两个 args 和输出规模。

- [ ] **Step 4：替换旧 memory log 占位**

在 `pruneOldToolResults` 中：

- 构建 `lookup := buildToolCallLookup(messages)`。
- 旧 tool result 超过 200 字符时调用 `summarizeToolResult`。
- 删除 `truncatedPlaceholder` 和 `see memory logs` 文案。

- [ ] **Step 5：运行测试**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run "TestSummarize.*ToolResult|TestSanitizeToolPairs"
```

预期：通过。

- [ ] **Step 6：提交**

```powershell
git add internal/agent/compaction.go internal/agent/compaction_tool.go internal/agent/compaction_tool_test.go
git commit -m "feat: summarize old tool results locally"
```

---

### Task 5：LLM summary 重试与非 LLM 降级

**文件：**
- 修改：`internal/agent/compaction.go`
- 新增测试：`internal/agent/compaction_fallback_test.go`

- [ ] **Step 1：先写 retry/fallback 测试**

创建 `internal/agent/compaction_fallback_test.go`：

```go
package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/provider"
)

type flakySummarizer struct {
	calls     int
	failCount int
}

func (f *flakySummarizer) Chat(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.Response, error) {
	f.calls++
	if f.calls <= f.failCount {
		return nil, errors.New("temporary summary failure")
	}
	return &provider.Response{Content: "llm summary"}, nil
}

func (f *flakySummarizer) ChatStream(context.Context, []provider.Message, []provider.Tool, string, int, float64) (*provider.StreamReader, error) {
	return nil, nil
}

func TestSummaryRetriesThenSucceeds(t *testing.T) {
	msgs := longConversation(10, 300)
	f := &flakySummarizer{failCount: 2}

	out, err := compressOlderMessages(msgs, CompactOptions{
		Provider:          f,
		Model:             "fake-model",
		TailTurns:         4,
		MinTailTurns:      2,
		SummaryMaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("compress: %v", err)
	}
	if f.calls != 3 {
		t.Fatalf("summary calls = %d, want 3", f.calls)
	}
	if !strings.Contains(out[0].Content, "llm summary") {
		t.Fatalf("missing LLM summary: %q", out[0].Content)
	}
}

func TestSummaryFallsBackAfterThreeFailures(t *testing.T) {
	msgs := longConversation(10, 300)
	f := &flakySummarizer{failCount: 3}

	out, err := compressOlderMessages(msgs, CompactOptions{
		Provider:          f,
		Model:             "fake-model",
		TailTurns:         4,
		MinTailTurns:      2,
		SummaryMaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("fallback should not return error: %v", err)
	}
	if f.calls != 3 {
		t.Fatalf("summary calls = %d, want 3", f.calls)
	}
	if !strings.Contains(out[0].Content, "deterministic fallback") {
		t.Fatalf("missing fallback marker: %q", out[0].Content)
	}
}

func longConversation(turns int, chars int) []provider.Message {
	msgs := make([]provider.Message, 0, turns*2)
	for i := 0; i < turns; i++ {
		msgs = append(msgs,
			provider.Message{Role: "user", Content: strings.Repeat("u", chars)},
			provider.Message{Role: "assistant", Content: strings.Repeat("a", chars)},
		)
	}
	return msgs
}
```

- [ ] **Step 2：运行测试确认失败**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run "TestSummary"
```

预期：失败，因为 retry/fallback 未实现。

- [ ] **Step 3：实现 retry**

在 `internal/agent/compaction.go` 增加：

```go
func summarizeWithRetries(opts CompactOptions, prompt []provider.Message) (string, error) {
	var lastErr error
	for attempt := 0; attempt < opts.SummaryMaxRetries; attempt++ {
		resp, err := opts.Provider.Chat(context.Background(), prompt, nil, opts.Model, 2048, 0.3)
		if err == nil && resp != nil && strings.TrimSpace(resp.Content) != "" {
			return resp.Content, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errors.New("empty summary response")
		}
	}
	return "", lastErr
}
```

- [ ] **Step 4：实现 deterministic fallback**

```go
func deterministicSummaryFallback(messages []provider.Message) string {
	var b strings.Builder
	b.WriteString("deterministic fallback: LLM summary failed after retries. Older messages were compacted without an LLM.\n")
	for _, msg := range messages {
		if msg.Origin != provider.OriginUser {
			continue
		}
		text := msg.TextContent()
		if text == "" && msg.Role == "tool" {
			text = msg.Content
		}
		if text == "" {
			continue
		}
		b.WriteString("[")
		b.WriteString(msg.Role)
		b.WriteString("] ")
		b.WriteString(snippetForFallback(text))
		b.WriteString("\n")
		if b.Len() > 12000 {
			b.WriteString("[fallback summary truncated]\n")
			break
		}
	}
	return b.String()
}

func snippetForFallback(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= 220 {
		return string(runes)
	}
	n := len(runes) / 10
	if n < 200 {
		n = 200
	}
	if n > 1000 {
		n = 1000
	}
	return string(runes[:n]) + "..."
}
```

- [ ] **Step 5：接入 `compressOlderMessages`**

LLM summary 失败后不再把错误冒泡为压缩失败，而是：

```go
summary, err := summarizeWithRetries(opts, summaryPrompt)
if err != nil {
	slog.Warn("LLM summary failed, using deterministic fallback", "error", err)
	summary = deterministicSummaryFallback(olderMessages)
}
```

- [ ] **Step 6：运行测试**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run "TestSummary"
```

预期：通过。

- [ ] **Step 7：提交**

```powershell
git add internal/agent/compaction.go internal/agent/compaction_fallback_test.go
git commit -m "feat: fall back when summary compaction fails"
```

---

### Task 6：手动 `/compact` 检查点模式

**文件：**
- 修改：`internal/agent/slash.go`
- 修改：`internal/agent/compaction.go`
- 修改测试：`internal/agent/compaction_test.go`

- [ ] **Step 1：先写 manual mode 测试**

追加到 `internal/agent/compaction_test.go`：

```go
func TestManualCompactionRunsBelowProactiveThreshold(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "short but old context"},
		{Role: "assistant", Content: "short reply"},
		{Role: "user", Content: "current task"},
	}
	f := &fakeSummarizer{}

	out, err := CompactMessagesWithOptions(msgs, CompactOptions{
		Mode:              CompactModeManual,
		Provider:          f,
		Model:             "fake-model",
		ContextWindow:     1000000,
		MaxOutputTokens:   1000,
		Focus:             "focus on filesystem changes",
		TailTurns:         1,
		MinTailTurns:      1,
		SummaryMaxRetries: 3,
	})
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	if !out.Pruned {
		t.Fatal("manual compaction should force a checkpoint summary")
	}
	if !strings.Contains(f.gotSummaryRequest, "focus on filesystem changes") {
		t.Fatalf("manual focus missing from summary request: %s", f.gotSummaryRequest)
	}
}
```

- [ ] **Step 2：运行测试确认失败**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run TestManualCompactionRunsBelowProactiveThreshold
```

预期：失败，直到 manual mode 绕过阈值并带入 focus。

- [ ] **Step 3：summary prompt 加入 manual focus**

summary prompt 构造时加入：

```go
if opts.Mode == CompactModeManual && strings.TrimSpace(opts.Focus) != "" {
	userPrompt.WriteString("Manual compaction focus:\n")
	userPrompt.WriteString(strings.TrimSpace(opts.Focus))
	userPrompt.WriteString("\n\n")
}
```

manual mode 的行为：

- 不看 proactive 阈值。
- 只要有 older region 可以折叠，就生成 checkpoint summary。
- 若消息太少，没有可折叠旧区，则返回 unchanged。

- [ ] **Step 4：slash command 接入 manual mode**

在 `handleSlashCommand` 中把：

```go
case "compact":
	return a.slashCompact(msg)
```

改为：

```go
case "compact":
	return a.slashCompact(msg, strings.Join(args, " "))
```

签名改为：

```go
func (a *Agent) slashCompact(msg bus.InboundMessage, focus string) slashResult
```

内部调用：

```go
result, err := CompactMessagesWithOptions(sessionMsgs, CompactOptions{
	Mode:              CompactModeManual,
	Workspace:         a.homePath,
	Provider:          a.provider,
	Model:             a.model,
	ContextWindow:     a.contextWindow,
	MaxOutputTokens:   a.maxTokens,
	Focus:             focus,
	TailTurns:         4,
	MinTailTurns:      2,
	SummaryMaxRetries: 3,
})
```

回复改为明确 checkpoint：

```go
return slashResult{handled: true, reply: fmt.Sprintf("Compacted checkpoint: %d -> %d messages.", len(sessionMsgs), len(result.Messages))}
```

- [ ] **Step 5：运行测试**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent
```

预期：通过。

- [ ] **Step 6：提交**

```powershell
git add internal/agent/slash.go internal/agent/compaction.go internal/agent/compaction_test.go
git commit -m "feat: make slash compact create checkpoints"
```

---

### Task 7：预判性压缩与紧急压缩接入 agent loop

**文件：**
- 修改：`internal/agent/loop.go`
- 修改：`internal/agent/slash.go`
- 修改：`internal/agent/compaction.go`
- 修改测试：`internal/agent/compaction_fallback_test.go`

- [ ] **Step 1：先写 context limit error detector 测试**

追加到 `internal/agent/compaction_fallback_test.go`：

```go
func TestIsContextLimitError(t *testing.T) {
	cases := []string{
		"context_length_exceeded",
		"maximum context length is 128000 tokens",
		"prompt is too long",
		"too many tokens in request",
		"input length exceeds context window",
	}
	for _, tc := range cases {
		if !isContextLimitError(errors.New(tc)) {
			t.Fatalf("expected context limit error for %q", tc)
		}
	}
	if isContextLimitError(errors.New("rate limit exceeded")) {
		t.Fatal("rate limit must not be treated as context limit")
	}
}
```

- [ ] **Step 2：运行测试确认失败**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run TestIsContextLimitError
```

预期：失败，因为 detector 尚未存在。

- [ ] **Step 3：实现 context limit detector 和 emergency compression**

在 `internal/agent/compaction.go` 中增加：

```go
func isContextLimitError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	needles := []string{
		"context_length_exceeded",
		"context length",
		"context window",
		"maximum context",
		"prompt is too long",
		"too many tokens",
		"input length",
		"request too large",
		"exceeds context",
	}
	for _, needle := range needles {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}
```

紧急压缩使用非 LLM 策略：

- 先 `sanitizeToolPairs`。
- 丢弃最旧约一半完整 turn。
- 插入一条用户消息说明“已在 context-limit error 后进行非 LLM 紧急压缩”。
- 再次 `sanitizeToolPairs`。

核心结构：

```go
func emergencyCompactMessages(messages []provider.Message, opts CompactOptions, beforeTokens int) *CompactResult {
	messages = sanitizeToolPairs(messages)
	if len(messages) <= 2 {
		return &CompactResult{Messages: messages}
	}

	cutoff := emergencyDropCutoff(messages)
	dropped := cutoff
	out := make([]provider.Message, 0, len(messages)-cutoff+1)
	out = append(out, provider.Message{
		Role: "user",
		Content: fmt.Sprintf("[Context compressed without LLM]\nEmergency compression dropped %d oldest messages after a context-limit error. Continue from the preserved recent conversation below.", dropped),
	})
	out = append(out, messages[cutoff:]...)
	out = sanitizeToolPairs(out)
	return &CompactResult{Messages: out, Pruned: true}
}
```

- [ ] **Step 4：Agent 增加 context metadata**

在 `Agent` struct 增加：

```go
contextWindow  int
providerConfigs map[string]config.ProviderConfig
```

构造和 `UpdateConfig` 中设置：

```go
contextWindow:   rc.ContextWindow,
providerConfigs: rc.Providers,
```

若 `rc.ContextWindow` 为 0：

```go
a.contextWindow = config.ResolveContextWindow(a.providerConfigs, a.model, a.maxTokens)
```

`/model` 切换后刷新：

```go
a.contextWindow = config.ResolveContextWindow(a.providerConfigs, a.model, a.maxTokens)
```

- [ ] **Step 5：LLM request overhead 在压缩前构建**

非流式和流式路径都改成：

```go
overheadMessages := a.buildRequestOverhead(systemPrompt, msg, chatterMem)
toolDefs := a.registry.DefinitionsForMode(builtinAllowForMode(a.promptMode))

compactResult, err := CompactMessagesWithOptions(sessionMsgs, CompactOptions{
	Mode:             CompactModeProactive,
	Workspace:        a.homePath,
	Provider:         a.provider,
	Model:            a.model,
	ContextWindow:    a.contextWindow,
	MaxOutputTokens:  a.maxTokens,
	OverheadMessages: overheadMessages,
	ToolDefs:         toolDefs,
	TailTurns:        4,
	MinTailTurns:     2,
	SummaryMaxRetries: 3,
})
```

然后组合：

```go
messages := append([]provider.Message{}, overheadMessages...)
messages = append(messages, sessionMsgs...)
```

`buildRequestOverhead` 应复用现有逻辑：

- system prompt
- channel hints
- sender metadata
- client params
- chatbot persistence reminder

- [ ] **Step 6：context limit error 后重试一次**

在每个 LLM call error branch 里，在返回通用错误前加入：

```go
if isContextLimitError(err) && !emergencyRetried {
	emergencyRetried = true
	result, compactErr := CompactMessagesWithOptions(sess.GetMessages(), CompactOptions{
		Mode:             CompactModeEmergency,
		Workspace:        a.homePath,
		Provider:         a.provider,
		Model:            a.model,
		ContextWindow:    a.contextWindow,
		MaxOutputTokens:  a.maxTokens,
		OverheadMessages: overheadMessages,
		ToolDefs:         toolDefs,
	})
	if compactErr == nil && result != nil && result.Pruned {
		sess.ReplaceMessages(result.Messages)
		sessionMsgs = result.Messages
		messages = append([]provider.Message{}, overheadMessages...)
		messages = append(messages, sessionMsgs...)
		continue
	}
}
```

在模型调用循环前声明：

```go
emergencyRetried := false
```

- [ ] **Step 7：运行聚焦测试**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/agent -run "TestIsContextLimitError|TestSummary|TestProactive|TestManual|TestSanitize"
```

预期：通过。

- [ ] **Step 8：提交**

```powershell
git add internal/agent/loop.go internal/agent/slash.go internal/agent/compaction.go internal/agent/compaction_fallback_test.go
git commit -m "feat: retry with emergency context compression"
```

---

### Task 8：最终验证

**文件：**
- 本计划前面所有修改文件。

- [ ] **Step 1：格式化**

```powershell
gofmt -w internal/config/config.go internal/config/context_window_test.go internal/gateway/userspace.go internal/agent/compaction.go internal/agent/compaction_test.go internal/agent/compaction_budget_test.go internal/agent/compaction_tool.go internal/agent/compaction_tool_test.go internal/agent/compaction_fallback_test.go internal/agent/loop.go internal/agent/slash.go
```

预期：无输出。

- [ ] **Step 2：运行相关包测试**

```powershell
New-Item -ItemType Directory -Force D:\tmp\ctxmode-go-cache, D:\tmp\ctxmode-go-tmp | Out-Null
$env:GOCACHE = "D:\tmp\ctxmode-go-cache"
$env:GOTMPDIR = "D:\tmp\ctxmode-go-tmp"
go test ./internal/config ./internal/gateway ./internal/agent
```

预期：通过。

- [ ] **Step 3：记录跳过全量测试**

不要运行：

```powershell
go test ./...
```

原因：本机安全软件会拦截带 `claw` 的 exe 和路径，全量测试可能编译或运行 `cmd/bkcrab` 相关 test binary。最终说明中记录：

- 全量测试按本机安全约束跳过。
- 已运行 `./internal/config ./internal/gateway ./internal/agent`。
- main 基线已有失败：`internal/setup` 缺 web embed 文件，`internal/agent/tools` 的 identity gate 测试失败。

- [ ] **Step 4：检查 diff**

```powershell
git diff --stat
git diff -- internal/agent internal/config internal/gateway docs/superpowers/plans/2026-06-17-context-compaction-modes.md
```

确认：

- compaction 代码不再出现 `memory log` 占位说明。
- `/compact` 使用 `CompactModeManual`。
- 预判性压缩使用百分比阈值和 `ContextWindow`。
- context-limit error 只触发一次 emergency compression retry。
- LLM summary 最多重试 3 次，然后 deterministic fallback。
- tool result 摘要由本地硬规则生成。

- [ ] **Step 5：最终提交**

若 Task 8 产生了格式化或小修正：

```powershell
git add internal/config internal/gateway internal/agent docs/superpowers/plans/2026-06-17-context-compaction-modes.md
git commit -m "test: cover context compaction modes"
```

若没有额外改动，则跳过提交并报告 working tree 状态。

---

## 自检

需求覆盖：

- `/compact` 主动压缩使用独立 manual checkpoint mode：Task 6。
- 预判性压缩从固定 80k 改为模型上下文百分比：Task 1、Task 2、Task 7。
- 最近上下文从固定 20 条消息改为以最近 4 个 user turn 为中心，最少保留 2 个 turn，并保持 tool pair 边界有效：Task 3。
- tool message 不再依赖 `memory_log`，旧 tool result 改成本地一行摘要：Task 4。
- LLM summary 失败重试 3 次，然后非 LLM fallback：Task 5。
- LLM 主调用返回上下文超限错误时触发紧急压缩并重试一次：Task 7。
- 测试环境中 `claw` 路径和 exe 拦截约束已写入测试环境硬约束和 Task 8。

类型一致性：

- `CompactMode`、`CompactOptions`、`CompactMessagesWithOptions`、`ContextWindow`、`ResolveContextWindow`、`RefreshModelContextWindow` 都在使用前定义。
- 测试 helper 名称与现有 `fakeSummarizer` 不冲突。

执行选择：

- 推荐使用 Subagent-Driven：按 Task 1 到 Task 8 逐个执行，每个 task 后主 agent 做 review。
- 也可以使用 Inline Execution：当前会话按计划逐项执行，并在关键任务后做 checkpoint。

