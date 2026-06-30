# 模型规格自动解析(contextWindow + maxTokens)— 设计

日期:2026-06-30
分支:`worktree-model-spec-autoresolve`

## 背景与问题

聊天页脚的"上下文占用"指示器显示 `66k / 128k tokens`。这个 `128k` 来自
[`config.ResolveContextWindow`](../../../internal/config/config.go) 的写死兜底值
`DefaultContextWindow = 128000`:当用户配置的模型条目没有显式填写
`contextWindow` 时,解析就回退到这个默认值。

用户配置 GLM 系列(如 `glm-5.1`,经 opencode zen 网关)时并未手填 `contextWindow`,
于是被压到 128K,而其真实窗口约 200K(models.dev 记为 202752),接近腰斩。

同时,模型配置里的 `ModelEntry.MaxTokens` 字段目前**未接入任何请求路径**——它只是
模型配置页上的 UI 元数据。真正生效(发给 provider 的"本次最大输出"、并参与压缩
预算 `MaxOutputTokens`)的是 per-agent 的输出上限 `rc.MaxTokens`,默认 8192,
与所选模型无关。

## 目标

用户照旧只填模型名 / apiBase / key,**系统内部自动解析模型的上下文窗口与最大输出
上限**,不再要求手填。

非目标(本期不做):
- `/v1/models` 模型下拉发现(只是省手敲模型名,与本特性正交,后续单独做)。
- 自动填充定价 `cost`(models.dev 是官方标价,可能与用户实际套餐/渠道价不符)。
- 运行时实时拉取 / 后台刷新。

## 关键决策(已与用户确认)

1. **数据源 = 内置快照(go:embed)**,运行时零联网。刷新靠重新生成并提交。
2. **解析范围 = contextWindow + maxTokens**(两者都要,见决策 3 的 maxTokens 语义)。
3. **maxTokens 真正接通生效(选项 a)**:对没有显式设过 maxTokens 的模型,有效
   输出上限从 8192 提升到模型上限(如 GLM 约 64000)。这是真实行为变化(更长输出、
   更高单次成本),用户已知悉并接受。自动值**来自 catalog 查表**,**不引入 per-model
   的 `ModelEntry.MaxTokens`**(决策 Z,见下)。
4. **决策 Z:后端最小改,不消费 per-model 字段**。`ModelEntry.MaxTokens` 当前未被
   后端读取,且模型配置页对它(及 `contextWindow`)预填默认值(8192 / 200000),
   若把该字段排在 catalog 之上会让"未改过默认值"的用户旁路掉自动解析。因此 maxTokens
   一律不读 `ModelEntry.MaxTokens`;contextWindow 维持只读 `ModelEntry.ContextWindow`
   的现状。代价见"已知限制"。
5. **纯运行时回退,不写回配置**:解析在 `ResolveContextWindow` / 新增的
   `ResolveMaxOutputTokens` 内完成,不改 save 流程、不写 DB。用户仍能在聊天页脚
   tooltip 看到解析后的窗口数值,反馈足够。

## 架构

### 新增包:`internal/modelspec`

职责单一:持有 models.dev 的精简快照,提供按模型 id 的查表。

- **精简快照** `internal/modelspec/catalog.json`(`//go:embed catalog.json`):
  不内嵌 2.4MB 的原始 `api.json`(其中定价、模态、知识截止日期等本特性用不上)。
  改为内嵌一份精简表,每条记录形如:
  ```json
  { "id": "glm-5.1", "name": "GLM 5.1", "context": 202752, "output": 65536, "apiHost": "opencode.ai" }
  ```
  预计几十 KB。`apiHost` 用于歧义消解(见"匹配规则")。

- **生成器** `internal/modelspec/gen/`(由 `//go:generate` 指令 + Makefile 目标
  `make modelspec` 驱动):联网拉取 `https://models.dev/api.json`,把
  `provider → models` 扁平化为精简表,写出 `catalog.json`。**联网只发生在开发机
  生成快照时,运行时永不联网。** 刷新 = 重跑生成器并提交新的 `catalog.json`。

- **加载与索引**:首次使用时 `sync.Once` 解析 `catalog.json` 并建立
  `id(小写) → []entry` 索引(同一 id 可能来自多个 provider)。解析失败时退化为
  空表(见"错误处理"),绝不 panic。

- **公开 API**:
  ```go
  // Spec 是某模型在 models.dev 上的精简规格。
  type Spec struct { ContextWindow int; MaxOutputTokens int }

  // Lookup 按模型 id(大小写不敏感)查表。apiHost 用于在同名多 provider 时
  // 择优;传空串则跳过 host 偏好、直接走保守兜底。ok=false 表示未收录。
  func Lookup(modelID, apiHost string) (Spec, bool)
  ```

### 接入点:`internal/config`

两处解析都改为"用户显式值 > ModelEntry 字段 > modelspec 查表 > 默认值"。

- **contextWindow** —— 扩展现有
  [`ResolveContextWindow`](../../../internal/config/config.go):
  在"用户填的 `entry.ContextWindow`(>0)"与"`fallbackContextWindow` 默认 128000"
  之间插入一层 `modelspec.Lookup(...).ContextWindow`。
  优先级:`entry.ContextWindow(>0)` → `modelspec 查表` → `默认 128000`。
  解析时把用户该 provider 的 apiBase 的主机名传给 `Lookup` 做歧义消解。

- **maxTokens(有效输出上限)** —— 新增 `ResolveMaxOutputTokens`:
  ```go
  func ResolveMaxOutputTokens(providers map[string]ProviderConfig, model string, explicit int) int
  ```
  优先级(决策 Z:**不读** per-model 的 `ModelEntry.MaxTokens`):
  1. `explicit`(agent 层用户显式设置的输出上限:file > agent entry > 全局 defaults,任一 >0)
  2. `modelspec.Lookup(...).MaxOutputTokens`(>0)
  3. 硬兜底 `8192`

  **关键约束**:必须能区分"用户显式设了 8192"与"默认回退到 8192"。当前
  `MergedAgentConfig` 先铺默认 8192 再被覆盖,无法区分。实现时需把 catalog 查表放在
  "应用 8192 硬默认"**之前**,即 8192 仅当 agent 层未显式设置且查表未命中时才用。
  解析出的有效值同时供 provider 请求与压缩预算(二者已共用 `rc.MaxTokens`)。

### 匹配规则

- 归一化:`TrimSpace` + 大小写不敏感;沿用现有 `provider/model` 前缀剥离逻辑后用
  裸 id 查表;id 不中时退一步按 `name` 匹配。
- **歧义消解**(同一 id 出现在 models.dev 多个 provider 下、数值不一致时,
  如 `glm-4.5` 有 131072 / 128000):
  1. 优先选 `apiHost` 与用户该 provider 的 apiBase 主机名匹配的那条;
  2. 无 host 匹配时,取**较保守(较小)的 context**——宁可低估窗口(顶多早一点
     触发压缩),也不高估导致请求超限报错。output 同理取保守值。

## 数据流

```
请求到来 → MergedAgentConfig / RuntimeConfig 解析
   ├─ ContextWindow = ResolveContextWindow(providers, model, maxTokens)
   │     entry.ContextWindow>0 ? → modelspec.Lookup.context>0 ? → 128000
   └─ MaxTokens     = ResolveMaxOutputTokens(providers, model, explicitMaxTokens)
         explicit>0 ? → modelspec.Lookup.output>0 ? → 8192
   ↓
loop.go 使用 a.contextWindow / a.maxTokens(发请求 + 压缩预算)
   ↓
usage 事件回传 contextWindow → 前端页脚 tooltip 显示真实窗口
```

运行时无任何网络调用;`catalog.json` 在二进制内。

## 错误处理

- `catalog.json` 解析失败 → 记一条 warn 日志 + 当作空表 → 完全回退到现状默认值
  (128000 / 8192),绝不 panic、不阻断启动。
- 模型未收录 → `Lookup` 返回 `ok=false` → 走现有兜底。
- 生成器联网失败 → 仅影响开发机刷新快照,报清晰错误并保留旧 `catalog.json`;
  不影响运行时。

## 测试

- **`internal/modelspec`**(用小的测试 fixture 表,不依赖全量内嵌快照):
  - 命中:`glm-5.1 → {202752, ...}`。
  - 大小写不敏感、`provider/` 前缀剥离、id 不中时按 name 命中。
  - 歧义消解:apiHost 匹配优先;无匹配取较小值。
  - 未收录 → `ok=false`。
  - 坏 JSON 快照 → 空表、不 panic。
- **`internal/config`**(扩展
  [`context_window_test.go`](../../../internal/config/context_window_test.go)):
  - 用户没填时 `glm-5.1` 经查表解析为 202752;用户显式 `entry.ContextWindow` 仍优先;
    未知模型仍回 128000。
  - `ResolveMaxOutputTokens` 三级优先级各一例;尤其验证"agent 层显式 8192 被尊重"
    与"未设时被模型上限覆盖"两种路径可区分。

## 影响面 / 风险

- 行为变化:未显式设 maxTokens 的模型,单次最大输出上限上升(8192 → 模型上限)。
  已确认接受。文档/changelog 需提示。
- 二进制体积:+几十 KB(精简快照)。可忽略。
- 启动:首次查表多一次 JSON 解析(`sync.Once`,几十 KB),可忽略。
- 数据时效:快照随构建固化;数值偏差风险低且有用户手填覆盖兜底。

## 已知限制(决策 Z 的代价)

- **UI 预填会旁路 contextWindow 查表**:模型配置页新建模型时预填 `contextWindow: 200000`
  ([web/.../models/page.tsx](../../../web/src/app/agents/[id]/models/page.tsx))。一旦该值
  写入配置则 `>0` 成立,`ResolveContextWindow` 取它而不查 catalog。因此本特性对
  "通过当前 UI 新建、且接受了 200000 预填"的模型不生效;对 `ContextWindow=0`(如用户
  当前的 glm-5.1、CLI/种子创建的条目)生效。彻底修复需把 UI 默认改为留空(方案 X),
  本期不做,留作后续。
- **per-model maxTokens 手动覆盖能力不做**:`ModelEntry.MaxTokens` 维持未被后端读取的
  现状(它本就不生效)。需要 per-model 覆盖输出上限的用户仍走 agent 层的 maxTokens。

## 范围外(后续可单独立项)

- `/v1/models` 模型 id 下拉发现(实测 zen 该端点仅返回 id 列表,可省手敲模型名)。
- 自动填充 `cost` 定价。
- 运行时实时刷新 / 后台拉新覆盖。
