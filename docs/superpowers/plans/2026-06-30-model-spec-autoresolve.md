# 模型规格自动解析 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让系统从内置的 models.dev 快照自动解析模型的上下文窗口与最大输出上限,用户不再手填。

**Architecture:** 新增 `internal/modelspec` 包,内嵌一份精简的 models.dev 快照(`//go:embed catalog.json`),提供按模型 id 的纯运行时查表 `Lookup`。`internal/config` 的 `ResolveContextWindow` 增加一层 catalog 兜底;新增 `ResolveMaxOutputTokens` 并接进 `MergedAgentConfig`,把系统默认 8192 降为查表之后的终极兜底。决策 Z:不消费 per-model 的 `ModelEntry.MaxTokens`。

**Tech Stack:** Go 1.25,`go:embed`,标准库 `encoding/json` / `net/url` / `sync`。

> **实现者须知 — maxTokens 的"显式"口径**:设计文档把 "agent 层显式(file > agent entry > 全局 defaults)" 列为最高优先级。但 `ApplyDefaults` 会把全局 `Agents.Defaults.MaxTokens` 强制成 8192,使其无法与"用户真设了 8192"区分。按设计文档"关键约束"的本意(8192 不得遮蔽查表),本计划把**仅 per-agent entry 与 per-file** 视为"显式"(>0 即用户设),把全局默认值当作**查表之后的终极兜底**。这是对 spec 文字的合理收口,不改 `ApplyDefaults`。

---

## File Structure

- **Create** `internal/modelspec/modelspec.go` — `Spec`/`Entry`/`Catalog` 类型、`Load([]byte)`、`(*Catalog).Lookup`、包级 `Lookup` + `//go:embed catalog.json`。
- **Create** `internal/modelspec/catalog.json` — 内嵌的精简快照(先放空壳,Task 2 由生成器写入真实数据)。
- **Create** `internal/modelspec/modelspec_test.go` — 用内存 fixture 测试解析与查表逻辑。
- **Create** `internal/modelspec/gen/main.go` — 生成器:拉 `models.dev/api.json` → 扁平化 → 写 `catalog.json`。
- **Modify** `internal/config/config.go` — 加 `DefaultMaxOutputTokens` 常量、`apiBaseHost`、`modelBareIDAndHost`、`ResolveMaxOutputTokens`;增强 `ResolveContextWindow`;重构 `MergedAgentConfig` 的 maxTokens 解析。
- **Modify** `internal/config/context_window_test.go` — 增加 catalog 兜底与 `ResolveMaxOutputTokens` 的测试。

依赖方向:`config` → `modelspec`(单向,无环;`modelspec` 不 import `config`)。

---

## Task 1: modelspec 包 — 类型、Load、Lookup(纯逻辑,fixture 测试)

**Files:**
- Create: `internal/modelspec/catalog.json`
- Create: `internal/modelspec/modelspec.go`
- Test: `internal/modelspec/modelspec_test.go`

- [ ] **Step 1: 创建空壳快照,使 go:embed 能编译**

创建 `internal/modelspec/catalog.json`,内容仅一行:

```json
{"entries":[]}
```

- [ ] **Step 2: 写失败测试**

创建 `internal/modelspec/modelspec_test.go`:

```go
package modelspec

import "testing"

const fixture = `{"entries":[
  {"id":"glm-5.1","name":"GLM 5.1","context":202752,"output":65536,"apiHost":"opencode.ai"},
  {"id":"glm-5.1","name":"GLM 5.1","context":200000,"output":64000,"apiHost":"bigmodel.cn"},
  {"id":"claude-sonnet-4","name":"Claude Sonnet 4","context":200000,"output":64000,"apiHost":"anthropic.com"}
]}`

func mustLoad(t *testing.T) *Catalog {
	t.Helper()
	c, err := Load([]byte(fixture))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func TestLookupByIDCaseInsensitive(t *testing.T) {
	c := mustLoad(t)
	spec, ok := c.Lookup("GLM-5.1", "opencode.ai")
	if !ok {
		t.Fatal("expected hit")
	}
	if spec.ContextWindow != 202752 || spec.MaxOutputTokens != 65536 {
		t.Fatalf("got %+v, want {202752 65536}", spec)
	}
}

func TestLookupApiHostPreferred(t *testing.T) {
	c := mustLoad(t)
	spec, _ := c.Lookup("glm-5.1", "bigmodel.cn")
	if spec.ContextWindow != 200000 {
		t.Fatalf("apiHost preference failed: got context %d, want 200000", spec.ContextWindow)
	}
}

func TestLookupConservativeWhenNoHostMatch(t *testing.T) {
	c := mustLoad(t)
	// 空 apiHost → 不偏好,取较保守(较小)值。
	spec, _ := c.Lookup("glm-5.1", "")
	if spec.ContextWindow != 200000 || spec.MaxOutputTokens != 64000 {
		t.Fatalf("conservative tie-break failed: got %+v, want {200000 64000}", spec)
	}
}

func TestLookupByNameWhenIDMisses(t *testing.T) {
	c := mustLoad(t)
	spec, ok := c.Lookup("Claude Sonnet 4", "")
	if !ok || spec.ContextWindow != 200000 {
		t.Fatalf("name lookup failed: ok=%v spec=%+v", ok, spec)
	}
}

func TestLookupUnknownReturnsFalse(t *testing.T) {
	c := mustLoad(t)
	if _, ok := c.Lookup("no-such-model", ""); ok {
		t.Fatal("expected miss")
	}
}

func TestLoadBadJSONErrors(t *testing.T) {
	if _, err := Load([]byte("{not json")); err == nil {
		t.Fatal("expected error on bad json")
	}
}
```

- [ ] **Step 3: 运行测试,确认编译失败(类型/函数未定义)**

Run: `go test ./internal/modelspec/`
Expected: FAIL — `undefined: Load` / `undefined: Catalog`。

- [ ] **Step 4: 实现 modelspec.go**

创建 `internal/modelspec/modelspec.go`:

```go
// Package modelspec 提供一份内嵌的、来自 models.dev 的精简模型规格快照,
// 用于在用户未显式配置时自动解析模型的上下文窗口与最大输出上限。
// 运行时零联网;快照由 ./gen 生成器离线刷新。
package modelspec

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

//go:embed catalog.json
var embedded []byte

// Spec 是某模型的精简规格。字段为 0 表示快照未收录该项。
type Spec struct {
	ContextWindow   int
	MaxOutputTokens int
}

// Entry 是快照里的一条记录(同一 id 可能来自多个 provider,各一条)。
type Entry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Context int    `json:"context"`
	Output  int    `json:"output"`
	APIHost string `json:"apiHost"`
}

type catalogFile struct {
	Entries []Entry `json:"entries"`
}

// Catalog 是按 id / name 建好索引的查表结构。
type Catalog struct {
	byID   map[string][]Entry
	byName map[string][]Entry
}

// Load 解析快照字节并建立索引。
func Load(data []byte) (*Catalog, error) {
	var f catalogFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, err
	}
	c := &Catalog{byID: map[string][]Entry{}, byName: map[string][]Entry{}}
	for _, e := range f.Entries {
		e.APIHost = strings.ToLower(strings.TrimSpace(e.APIHost))
		if id := strings.ToLower(strings.TrimSpace(e.ID)); id != "" {
			c.byID[id] = append(c.byID[id], e)
		}
		if name := strings.ToLower(strings.TrimSpace(e.Name)); name != "" {
			c.byName[name] = append(c.byName[name], e)
		}
	}
	return c, nil
}

// Lookup 按模型 id(大小写不敏感)查表;id 不中时退一步按 name 查。
// apiHost 非空时优先选 host 匹配的记录;否则(或无匹配)取较保守的较小值。
// 返回 ok=false 表示未收录。
func (c *Catalog) Lookup(modelID, apiHost string) (Spec, bool) {
	if c == nil {
		return Spec{}, false
	}
	key := strings.ToLower(strings.TrimSpace(modelID))
	if key == "" {
		return Spec{}, false
	}
	cands := c.byID[key]
	if len(cands) == 0 {
		cands = c.byName[key]
	}
	if len(cands) == 0 {
		return Spec{}, false
	}

	if host := strings.ToLower(strings.TrimSpace(apiHost)); host != "" {
		var matched []Entry
		for _, e := range cands {
			if e.APIHost == host {
				matched = append(matched, e)
			}
		}
		if len(matched) > 0 {
			cands = matched
		}
	}

	var spec Spec
	for _, e := range cands {
		if e.Context > 0 && (spec.ContextWindow == 0 || e.Context < spec.ContextWindow) {
			spec.ContextWindow = e.Context
		}
		if e.Output > 0 && (spec.MaxOutputTokens == 0 || e.Output < spec.MaxOutputTokens) {
			spec.MaxOutputTokens = e.Output
		}
	}
	if spec.ContextWindow == 0 && spec.MaxOutputTokens == 0 {
		return Spec{}, false
	}
	return spec, true
}

// defaultCatalog 惰性解析内嵌快照一次;解析失败时返回 nil,Lookup 退化为未收录。
var defaultCatalog = sync.OnceValue(func() *Catalog {
	c, err := Load(embedded)
	if err != nil {
		return nil
	}
	return c
})

// Lookup 用内嵌快照查表,供 internal/config 调用。
func Lookup(modelID, apiHost string) (Spec, bool) {
	return defaultCatalog().Lookup(modelID, apiHost)
}
```

- [ ] **Step 5: 运行测试,确认通过**

Run: `go test ./internal/modelspec/`
Expected: PASS(全部用例)。

- [ ] **Step 6: 提交**

```bash
git add internal/modelspec/modelspec.go internal/modelspec/catalog.json internal/modelspec/modelspec_test.go
git commit -m "feat(modelspec): 内嵌模型规格查表(Load/Lookup,空壳快照)"
```

---

## Task 2: 生成器 — 拉取 models.dev 并写入真实快照

**Files:**
- Create: `internal/modelspec/gen/main.go`
- Modify: `internal/modelspec/catalog.json`(由生成器覆盖)
- Modify: `internal/modelspec/modelspec.go`(加 `//go:generate` 指令)

- [ ] **Step 1: 实现生成器**

创建 `internal/modelspec/gen/main.go`:

```go
// Command gen 从 models.dev 拉取模型规格,扁平化为精简快照写入 catalog.json。
// 仅供离线刷新使用(开发机联网运行);运行时不依赖它。
//
// 用法:在 internal/modelspec 目录执行 `go generate`,或直接
//   go run ./internal/modelspec/gen -out internal/modelspec/catalog.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

const source = "https://models.dev/api.json"

type mdevModel struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
}

type mdevProvider struct {
	ID     string               `json:"id"`
	Name   string               `json:"name"`
	API    string               `json:"api"`
	Models map[string]mdevModel `json:"models"`
}

type entry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Context int    `json:"context"`
	Output  int    `json:"output"`
	APIHost string `json:"apiHost"`
}

type catalogFile struct {
	Entries []entry `json:"entries"`
}

func apiHost(api string) string {
	api = strings.TrimSpace(api)
	if api == "" {
		return ""
	}
	if !strings.Contains(api, "://") {
		api = "https://" + api
	}
	u, err := url.Parse(api)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func main() {
	out := flag.String("out", "catalog.json", "output path for the slim catalog")
	flag.Parse()

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch %s: %v\n", source, err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "fetch %s: status %d\n", source, resp.StatusCode)
		os.Exit(1)
	}

	var providers map[string]mdevProvider
	if err := json.NewDecoder(resp.Body).Decode(&providers); err != nil {
		fmt.Fprintf(os.Stderr, "decode: %v\n", err)
		os.Exit(1)
	}

	var entries []entry
	for _, p := range providers {
		host := apiHost(p.API)
		for _, m := range p.Models {
			if m.Limit.Context <= 0 && m.Limit.Output <= 0 {
				continue
			}
			entries = append(entries, entry{
				ID:      m.ID,
				Name:    m.Name,
				Context: m.Limit.Context,
				Output:  m.Limit.Output,
				APIHost: host,
			})
		}
	}
	// 稳定排序,使快照 diff 可读、可复现。
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].ID != entries[j].ID {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].APIHost < entries[j].APIHost
	})

	buf, err := json.MarshalIndent(catalogFile{Entries: entries}, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	buf = append(buf, '\n')
	if err := os.WriteFile(*out, buf, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", *out, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d entries to %s\n", len(entries), *out)
}
```

- [ ] **Step 2: 在 modelspec.go 顶部加 go:generate 指令**

在 `internal/modelspec/modelspec.go` 的 `package modelspec` 行下方新增一行:

```go
//go:generate go run ./gen -out catalog.json
```

- [ ] **Step 3: 运行生成器,产出真实快照**

Run: `go run ./internal/modelspec/gen -out internal/modelspec/catalog.json`
Expected: 打印 `wrote <N> entries to internal/modelspec/catalog.json`(N 为几百级别)。

- [ ] **Step 4: 校验快照含预期数据**

Run: `go run ./internal/modelspec/gen -out /tmp/_check.json >/dev/null; grep -c '"glm-5.1"' internal/modelspec/catalog.json`
Expected: 输出 ≥ 1(快照里有 glm-5.1)。

- [ ] **Step 5: 复跑 modelspec 测试(应仍全绿,测试用 fixture 不依赖快照)**

Run: `go test ./internal/modelspec/`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add internal/modelspec/gen/main.go internal/modelspec/modelspec.go internal/modelspec/catalog.json
git commit -m "feat(modelspec): 新增 models.dev 生成器并产出真实快照"
```

---

## Task 3: 把 catalog 接进 config.ResolveContextWindow

**Files:**
- Modify: `internal/config/config.go`(`ResolveContextWindow` 约 324-342 行,新增 `apiBaseHost` / `modelBareIDAndHost` 辅助)
- Test: `internal/config/context_window_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/config/context_window_test.go` 末尾追加(顶部 import 改为 `import ("testing"; "github.com/qs3c/bkcrab/internal/modelspec")`):

```go
func TestResolveContextWindowUsesCatalogWhenEntryUnset(t *testing.T) {
	// 用户没填 entry.ContextWindow → 应回退到 models.dev 查表,而非 128000。
	want, ok := modelspec.Lookup("glm-5.1", "opencode.ai")
	if !ok || want.ContextWindow <= 0 {
		t.Fatalf("fixture/snapshot missing glm-5.1: ok=%v want=%+v", ok, want)
	}
	providers := map[string]ProviderConfig{
		"opencode": {
			APIBase: "https://opencode.ai/zen/v1",
			Models:  []ModelEntry{{ID: "glm-5.1", Name: "GLM 5.1"}}, // 注意:无 ContextWindow
		},
	}
	got := ResolveContextWindow(providers, "opencode/glm-5.1", 8192)
	if got != want.ContextWindow {
		t.Fatalf("context window = %d, want %d (from catalog)", got, want.ContextWindow)
	}
	if got == DefaultContextWindow {
		t.Fatalf("catalog layer not consulted: got default %d", got)
	}
}

func TestResolveContextWindowExplicitEntryStillWins(t *testing.T) {
	providers := map[string]ProviderConfig{
		"opencode": {
			APIBase: "https://opencode.ai/zen/v1",
			Models:  []ModelEntry{{ID: "glm-5.1", ContextWindow: 12345}},
		},
	}
	if got := ResolveContextWindow(providers, "opencode/glm-5.1", 8192); got != 12345 {
		t.Fatalf("explicit entry should win: got %d, want 12345", got)
	}
}

func TestResolveContextWindowUnknownStillFallsBack(t *testing.T) {
	if got := ResolveContextWindow(nil, "totally/unknown-model", 8192); got != DefaultContextWindow {
		t.Fatalf("got %d, want default %d", got, DefaultContextWindow)
	}
}
```

- [ ] **Step 2: 运行测试,确认失败**

Run: `go test ./internal/config/ -run TestResolveContextWindowUsesCatalog -v`
Expected: FAIL — `context window = 128000, want 202752`(catalog 层尚未接入)。

- [ ] **Step 3: 实现 catalog 层与辅助函数**

在 `internal/config/config.go` 顶部 import 区加入(保持分组):

```go
	"net/url"

	"github.com/qs3c/bkcrab/internal/modelspec"
```

把现有 `ResolveContextWindow`(约 323-335 行)整体替换为:

```go
// ResolveContextWindow 解析模型上下文窗口。优先级:
// 用户填写的 ModelEntry.ContextWindow(>0) > models.dev 查表 > 默认 128000。
func ResolveContextWindow(providers map[string]ProviderConfig, model string, maxTokens int) int {
	model = strings.TrimSpace(model)
	if model != "" && len(providers) > 0 {
		if contextWindow, ok := resolvePrefixedModelContextWindow(providers, model); ok {
			return contextWindow
		}
		if contextWindow, ok := resolveAnyProviderModelContextWindow(providers, model); ok {
			return contextWindow
		}
	}
	if bareID, apiHost := modelBareIDAndHost(providers, model); bareID != "" {
		if spec, ok := modelspec.Lookup(bareID, apiHost); ok && spec.ContextWindow > 0 {
			return spec.ContextWindow
		}
	}
	return fallbackContextWindow(maxTokens)
}

// apiBaseHost 从 provider 的 apiBase 提取小写主机名(容忍缺协议)。
func apiBaseHost(apiBase string) string {
	apiBase = strings.TrimSpace(apiBase)
	if apiBase == "" {
		return ""
	}
	if !strings.Contains(apiBase, "://") {
		apiBase = "https://" + apiBase
	}
	u, err := url.Parse(apiBase)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// modelBareIDAndHost 把用户的 model 字符串解析为 (裸模型 id, 该 provider 的 apiBase 主机名),
// 供 modelspec 查表与歧义消解使用。优先按 `providerID/` 前缀剥离;否则按
// Models 列表里 ID/Name 命中的 provider 取 host;再否则裸 id + 空 host。
func modelBareIDAndHost(providers map[string]ProviderConfig, model string) (string, string) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", ""
	}
	for _, providerID := range sortedProviderKeysByLength(providers) {
		prefix := providerID + "/"
		if strings.HasPrefix(model, prefix) {
			return strings.TrimPrefix(model, prefix), apiBaseHost(providers[providerID].APIBase)
		}
	}
	for _, providerID := range sortedProviderKeys(providers) {
		for _, entry := range providers[providerID].Models {
			if entry.ID == model || entry.Name == model {
				return model, apiBaseHost(providers[providerID].APIBase)
			}
		}
	}
	return model, ""
}
```

- [ ] **Step 4: 运行测试,确认通过**

Run: `go test ./internal/config/ -run TestResolveContextWindow -v`
Expected: PASS(含新旧全部 `ResolveContextWindow*` 用例)。

- [ ] **Step 5: 提交**

```bash
git add internal/config/config.go internal/config/context_window_test.go
git commit -m "feat(config): ResolveContextWindow 增加 models.dev 查表兜底"
```

---

## Task 4: 新增 config.ResolveMaxOutputTokens

**Files:**
- Modify: `internal/config/config.go`(新增常量与函数)
- Test: `internal/config/context_window_test.go`

- [ ] **Step 1: 写失败测试**

在 `internal/config/context_window_test.go` 末尾追加:

```go
func TestResolveMaxOutputTokensExplicitWins(t *testing.T) {
	providers := map[string]ProviderConfig{
		"opencode": {APIBase: "https://opencode.ai/zen/v1", Models: []ModelEntry{{ID: "glm-5.1"}}},
	}
	// explicit=16000(用户 per-agent 设的)应原样返回,不查表、不兜底。
	if got := ResolveMaxOutputTokens(providers, "opencode/glm-5.1", 16000, 8192); got != 16000 {
		t.Fatalf("explicit should win: got %d, want 16000", got)
	}
}

func TestResolveMaxOutputTokensUsesCatalogWhenNotExplicit(t *testing.T) {
	want, ok := modelspec.Lookup("glm-5.1", "opencode.ai")
	if !ok || want.MaxOutputTokens <= 0 {
		t.Fatalf("snapshot missing glm-5.1 output: ok=%v want=%+v", ok, want)
	}
	providers := map[string]ProviderConfig{
		"opencode": {APIBase: "https://opencode.ai/zen/v1", Models: []ModelEntry{{ID: "glm-5.1"}}},
	}
	// explicit=0(未显式设),fallback=8192(系统默认)→ 应取查表值,而非 8192。
	got := ResolveMaxOutputTokens(providers, "opencode/glm-5.1", 0, 8192)
	if got != want.MaxOutputTokens {
		t.Fatalf("should use catalog: got %d, want %d", got, want.MaxOutputTokens)
	}
	if got == 8192 {
		t.Fatal("system default 8192 wrongly shadowed catalog")
	}
}

func TestResolveMaxOutputTokensFallsBackWhenUnknown(t *testing.T) {
	got := ResolveMaxOutputTokens(nil, "totally/unknown-model", 0, 0)
	if got != DefaultMaxOutputTokens {
		t.Fatalf("got %d, want %d", got, DefaultMaxOutputTokens)
	}
}
```

- [ ] **Step 2: 运行测试,确认失败**

Run: `go test ./internal/config/ -run TestResolveMaxOutputTokens -v`
Expected: FAIL — `undefined: ResolveMaxOutputTokens` / `undefined: DefaultMaxOutputTokens`。

- [ ] **Step 3: 实现常量与函数**

在 `internal/config/config.go` 的 `const DefaultContextWindow = 128000` 行下方新增:

```go
// DefaultMaxOutputTokens 是所有解析都未命中时的最大输出兜底(与 ApplyDefaults 一致)。
const DefaultMaxOutputTokens = 8192
```

并在 `ResolveContextWindow` 相关函数附近新增:

```go
// ResolveMaxOutputTokens 解析模型有效最大输出上限。优先级:
// explicit(per-agent/file 用户显式,>0) > models.dev 查表 > fallback(系统默认,>0) > 8192。
// 注意:系统默认值作为终极兜底传入 fallback,因此不会遮蔽查表(见 plan 顶部说明)。
func ResolveMaxOutputTokens(providers map[string]ProviderConfig, model string, explicit, fallback int) int {
	if explicit > 0 {
		return explicit
	}
	if bareID, apiHost := modelBareIDAndHost(providers, model); bareID != "" {
		if spec, ok := modelspec.Lookup(bareID, apiHost); ok && spec.MaxOutputTokens > 0 {
			return spec.MaxOutputTokens
		}
	}
	if fallback > 0 {
		return fallback
	}
	return DefaultMaxOutputTokens
}
```

- [ ] **Step 4: 运行测试,确认通过**

Run: `go test ./internal/config/ -run TestResolveMaxOutputTokens -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add internal/config/config.go internal/config/context_window_test.go
git commit -m "feat(config): 新增 ResolveMaxOutputTokens(显式>查表>默认)"
```

---

## Task 5: 把 ResolveMaxOutputTokens 接进 MergedAgentConfig

**Files:**
- Modify: `internal/config/config.go`(`MergedAgentConfig`,约 766-846 行)
- Test: `internal/config/context_window_test.go`

> 现状:`MergedAgentConfig` 在第 773 行用 `cfg.Agents.Defaults.MaxTokens`(经 `ApplyDefaults` 已为 8192)给 `resolved.MaxTokens` 赋初值,随后第 782-784 行用 `entry.MaxTokens(>0)`、第 844-846 行用 `fileCfg.MaxTokens(>0)` 覆盖。本任务把这三处直连赋值改为:**捕获 per-agent/file 的显式值,在 fileCfg 解析完成后统一用 `ResolveMaxOutputTokens` 计算一次**,使系统默认 8192 降为查表之后的兜底。

- [ ] **Step 1: 写失败测试**

在 `internal/config/context_window_test.go` 末尾追加:

```go
func TestMergedAgentConfigMaxTokensFromCatalog(t *testing.T) {
	want, ok := modelspec.Lookup("glm-5.1", "opencode.ai")
	if !ok || want.MaxOutputTokens <= 0 {
		t.Fatalf("snapshot missing glm-5.1 output")
	}
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"opencode": {APIBase: "https://opencode.ai/zen/v1", Models: []ModelEntry{{ID: "glm-5.1"}}},
		},
	}
	cfg.Agents.Defaults.Model = "opencode/glm-5.1"
	ApplyDefaults(cfg) // 把 Defaults.MaxTokens 设为 8192
	rc := cfg.MergedAgentConfig(AgentEntry{ID: "a1"})
	if rc.MaxTokens != want.MaxOutputTokens {
		t.Fatalf("MaxTokens = %d, want %d (catalog), 系统默认 8192 不应遮蔽查表", rc.MaxTokens, want.MaxOutputTokens)
	}
}

func TestMergedAgentConfigExplicitMaxTokensWins(t *testing.T) {
	cfg := &Config{
		Providers: map[string]ProviderConfig{
			"opencode": {APIBase: "https://opencode.ai/zen/v1", Models: []ModelEntry{{ID: "glm-5.1"}}},
		},
	}
	cfg.Agents.Defaults.Model = "opencode/glm-5.1"
	ApplyDefaults(cfg)
	rc := cfg.MergedAgentConfig(AgentEntry{ID: "a1", MaxTokens: 4096})
	if rc.MaxTokens != 4096 {
		t.Fatalf("explicit per-agent MaxTokens should win: got %d, want 4096", rc.MaxTokens)
	}
}
```

- [ ] **Step 2: 运行测试,确认失败**

Run: `go test ./internal/config/ -run TestMergedAgentConfigMaxTokens -v`
Expected: FAIL — `MaxTokens = 8192, want 65536`(catalog 尚未接入 MergedAgentConfig)。

- [ ] **Step 3: 重构 MergedAgentConfig 的 maxTokens 解析**

在 `internal/config/config.go` 的 `MergedAgentConfig` 中:

(a) 删除第 773 行 `resolved` 结构体字面量里的 `MaxTokens: cfg.Agents.Defaults.MaxTokens,` 这一行(改由末尾统一计算)。

(b) 把第 782-784 行:

```go
	if entry.MaxTokens > 0 {
		resolved.MaxTokens = entry.MaxTokens
	}
```

替换为(用局部变量捕获显式值,暂不写入 resolved):

```go
	explicitMaxTokens := 0
	if entry.MaxTokens > 0 {
		explicitMaxTokens = entry.MaxTokens
	}
```

(c) 把第 844-846 行:

```go
		if fileCfg.MaxTokens > 0 {
			resolved.MaxTokens = fileCfg.MaxTokens
		}
```

替换为:

```go
		if fileCfg.MaxTokens > 0 {
			explicitMaxTokens = fileCfg.MaxTokens
		}
```

(d) 紧接 `if fileCfg, ok := AgentFileConfigLoader(...); ok { ... }` 这个块**结束之后**(即 model/maxTokens 的所有覆盖都已确定后),新增一次统一计算:

```go
	resolved.MaxTokens = ResolveMaxOutputTokens(
		resolved.Providers, resolved.Model, explicitMaxTokens, cfg.Agents.Defaults.MaxTokens,
	)
```

> 注意:`resolved.Providers` 在第 821-826 行已填充、`resolved.Model` 在第 772 行设初值并可能被 fileCfg 第 841-843 行覆盖,故该计算必须放在 fileCfg 块之后,确保拿到最终的 Model 与 Providers。

- [ ] **Step 4: 运行 config 全部测试,确认通过**

Run: `go test ./internal/config/`
Expected: PASS(含新用例与既有 `TestResolvedAgentRefreshModelContextWindow` 等)。

- [ ] **Step 5: 提交**

```bash
git add internal/config/config.go internal/config/context_window_test.go
git commit -m "feat(config): MergedAgentConfig 经 ResolveMaxOutputTokens 解析输出上限"
```

---

## Task 6: 全量回归与收尾

**Files:** 无新增改动(验证为主)

- [ ] **Step 1: 跑受影响包的测试**

Run: `go test ./internal/modelspec/ ./internal/config/ ./internal/agent/ ./internal/gateway/`
Expected: 全部 `ok`。

- [ ] **Step 2: 全量回归**

Run: `go test ./...`
Expected: 全部 `ok`(若 `internal/setup` / `cmd/bkcrab` 因缺 `internal/setup/web` 前端产物报 `pattern all:web: no matching files found`,属环境问题,与本特性无关;执行环境若已有该产物则应全绿)。

- [ ] **Step 3: 编译总入口,确认无引用遗漏**

Run: `go build ./...`
Expected: 无输出、退出码 0(同样地,`all:web` 产物缺失只影响 setup/cmd 两包)。

- [ ] **Step 4: 手动确认解析结果(可选)**

Run: `go test ./internal/config/ -run TestResolveContextWindowUsesCatalogWhenEntryUnset -v`
Expected: PASS —— 印证 `glm-5.1` 在未填 ContextWindow 时解析到 models.dev 的真实窗口(约 202752),而非 128000。

- [ ] **Step 5: 最终提交(若前几步有微调)**

```bash
git add -A
git commit -m "test(modelspec): 全量回归通过"
```

---

## Self-Review 记录

- **Spec 覆盖**:数据源内嵌快照(Task 1-2)、contextWindow 查表层(Task 3)、maxTokens 解析含决策 Z(Task 4-5)、纯运行时不写配置(全程未触 save/DB)、匹配规则与歧义消解(Task 1 的 Lookup + Task 3 的 modelBareIDAndHost)、错误处理(Load 失败→nil→未收录;Task 1 的 `TestLoadBadJSONErrors`)、测试(各 Task 内含)。已知限制(UI 预填、per-model 不消费)为设计层声明,无需任务。
- **占位符**:无 TBD/TODO;每个代码步骤均含完整代码与确切命令、预期输出。
- **类型一致性**:`Spec{ContextWindow,MaxOutputTokens}`、`Catalog`、`Load`、`Lookup`、`ResolveContextWindow`、`ResolveMaxOutputTokens(providers,model,explicit,fallback)`、`modelBareIDAndHost`、`apiBaseHost`、`DefaultMaxOutputTokens` 在各任务间签名一致。
