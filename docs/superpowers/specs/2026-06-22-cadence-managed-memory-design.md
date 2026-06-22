# 节拍记忆提取接入 managed 记忆引擎 — 设计文档

- 日期：2026-06-22
- 分支：`feat/managed-memory-tool`
- 状态：已评审，待实现计划

## 1. 背景与问题

本仓库有两条把对话事实写入聊天者 `USER.md` / `MEMORY.md` 的链路：

1. **交互链路**：对话主循环的 agent 在回合内自主调用内置 `memory` 工具，工具背后是 `internal/memory` 的 managed 引擎（`Manager.Apply`），支持 add/replace/remove，带唯一匹配、去重、字符上限、严格隐私扫描，写入走 `MutateAgentFile` 事务。

2. **节拍链路**：`internal/agent/memory.go` 的 `AutoPersistMemory`，由 `loop.go` 在每 N 轮（`AutoPersist.EveryNTurns`，经 `ClaimCadenceBatch` 认领批次）后台 goroutine 触发。它是一次 `tools=nil` 的一次性 JSON 补全，输出 `{"memory_facts":[...], "user_notes":[...]}`，Go 侧把结果**纯追加**（`## Auto-persisted: <时间戳>` + bullet）写回，没有去重、没有上限、没有 replace/remove。

节拍链路因此只会让记忆文件单调膨胀，且当前喂给模型的旧记忆只截前 500 字符、无法支持改写/删除。

此外，系统 prompt 注入存在一处不一致：`context.go` 注入 `USER.md` / `MEMORY.md` 时走 `LoadMemory` / `loadFileForUser` 读**原始文件字节**，未经过 manager 渲染，也未经严格扫描。结果是模型在系统 prompt 里会看到受管格式的存储编码（`<!-- bkclaw-memory:v1 -->` 标记与 `§` 分隔符），且 List 提供的消毒在主注入路径上被绕过。

## 2. 目标

1. 让节拍链路从"纯追加"改为"模型自主判断 add/replace/remove，经 `Manager.Apply` 安全写入"，真正止住膨胀。
2. 节拍 prompt 包含**完整**的 user/memory 条目视图（经 `Manager.List` 消毒），让模型既能 add，也能判断 replace / remove。
3. 修复系统 prompt 注入：`context.go` 注入 `USER.md` / `MEMORY.md` 改走 manager 渲染（消毒 + 去存储编码）。
4. 收口工具表面：`memory` 工具移除 `list` 动作；`list` 仅作为 manager 引擎能力存在。

## 3. 非目标 / 不在范围

- 不改 `internal/memory` 引擎的匹配/操作语义（`Operation` 结构、`Apply`、`List` 保持现状）。**不引入 index 寻址**（序号在批内 remove 时会漂移，复杂度更高；`old_text` 按内容匹配，天然无此问题）。
- 不改交互链路的定位方式（仍用 `old_text`）。
- 不动旧的关键词版 `Memory.ReviewAndUpdateMemory`（独立的 heartbeat 旁路）。
- 不对节拍超限做硬化重试逻辑（见 §7 边界，留作后续）。

## 4. 关键决策（已确认）

| 决策点 | 结论 |
|---|---|
| user/memory 操作的调用结构 | **一次 LLM 调用**，输出两组 ops（`memory_ops` + `user_ops`） |
| replace/remove 定位方式 | **`old_text` 整条原文匹配**，两条链路统一，不引入 index |
| 引擎改动 | **零改动**，复用 `Apply` 的 `uniqueSubstringMatch` + 去重 + 上限 + 扫描 |
| 输出 max_tokens | **固定 200 → 2048**（cap 非目标值、按实际输出计费，宽松防截断） |
| 提取所用模型 | 仍走 `AutoPersist.Model`（可配便宜模型） |
| `list` 工具动作 | **移除**；`Manager.List` 仅作引擎内部能力（render + 节拍 prompt 用） |
| context.go 注入 | `USER.md` / `MEMORY.md` 改走 manager 渲染；其它身份文件不动 |

## 5. 详细设计

### 5.1 引擎（`internal/memory`）

不改。`Operation{Action, Content, OldText}`、`Manager.Apply`、`Manager.List`、`Manager.Render` 全部沿用。`Manager.List` 返回的 `Result.Entries` 已是经 `safeEntriesForList` 严格扫描消毒（威胁条目替换为 `[BLOCKED: …]` 占位符）的条目，`Result.Usage` 给出 `X/上限 字符`。

### 5.2 节拍链路改造（`AutoPersistMemory` + 调用处）

**构造 Manager（调用处 `loop.go`）**：用 `a.dataStore` 经 `MemoryStoreAdapter` 适配为 `memory.Store`，以 `AgentID=a.name`、`UserID=chatterUID`、`Config=`（与 registry 一致的 managedMemoryCfg；若 agent 侧暂无该字段则取 `memory.DefaultConfig()`，作为实现细节在计划中确定来源）构造 `*memory.Manager`，传入 `AutoPersistMemory`。

**签名调整**：`AutoPersistMemory` 由收 `*Memory` 改为收 `*memory.Manager`（其余参数不变：ctx、provider、model、groups），便于注入与隔离测试。

**prompt 输入**：
- 用 `manager.List(ctx, TargetMemory)` 与 `manager.List(ctx, TargetUser)` 取得消毒后的**完整**条目视图（移除原 `truncateStr(..., 500)`）。
- 将两个目标的当前 `Usage`（`X/上限`）一并放入 prompt。
- 保留对话材料的输入侧上限 `maxPromptChars = 12000`（仅约束 turn 材料拼装），与完整记忆条目一起组成最终 prompt。
- 明确指示模型：接近字符上限时**优先用 replace/remove 压缩**而非一味 add；`old_text` 必须是所列某条目的**完整原文**（去重后整条必唯一，匹配最稳）。

**输出 schema**（一次调用两组）：

```json
{
  "memory_ops": [
    {"action": "add", "content": "..."},
    {"action": "replace", "old_text": "<要改的那条的完整原文>", "content": "..."},
    {"action": "remove", "old_text": "<要删的那条的完整原文>"}
  ],
  "user_ops": [ ... ]
}
```

沿用现有的 `stripJSONFence` 处理模型反射性包裹的 ```json 围栏。

**落盘**：解析 → 两组 `[]memory.Operation` → `manager.Apply(ctx, TargetMemory, memoryOps)` 与 `manager.Apply(ctx, TargetUser, userOps)`。删除原 `SaveMemoryWithScan` / `SaveUserFile` 的追加写回逻辑。

**max_tokens**：`prov.Chat(...)` 的第 5 参数由 `200` 改为 `2048`；temperature 维持 `0.3`。

### 5.3 工具表面收口（`internal/agent/tools/memory_tool.go`）

- `action` 枚举与 `operations[].action` 枚举去掉 `"list"`，仅 `["add","replace","remove"]`。
- 去掉"空 action 默认为 list"与单条 list → `manager.List` 的分发分支；`makeMemoryTool` 一律走 `Apply`。
- action 缺失/为空时返回明确错误（要求 add/replace/remove）。
- 更新工具 `description`，移除对 list 的描述。
- `Manager.List` / `ActionList` 等引擎符号保留不动（仍被 render 与节拍使用）。

### 5.4 系统 prompt 注入修复（`internal/agent/context.go`）

注入 `<current_chatter_profile>`（`USER.md`）与 `<chatter_long_term_memory>`（`MEMORY.md`）时，改为经 manager 渲染（消毒条目 + 去 `§`/标记编码），不再塞原始字节。**仅这两个文件**走此路径；`SOUL.md` / `IDENTITY.md` / `AGENTS.md` 等身份文件注入不变。空内容时的占位文案沿用现状（已在分支内改为引导 memory 工具）。

## 6. 数据流（节拍链路，改造后）

```
每 N 轮 → ClaimCadenceBatch 认领批次 → 后台 goroutine
  → LoadTurnMessages(refs) 取本批 turn
  → 构造 Manager(agentID, chatterUID, store, cfg)
  → manager.List(memory) + manager.List(user)            // 完整消毒条目 + Usage
  → 组装 prompt（条目视图 + Usage + turn 材料 + 指示）
  → prov.Chat(prompt, tools=nil, model, maxTokens=2048, 0.3)
  → stripJSONFence + 解析 {memory_ops, user_ops}
  → manager.Apply(memory, memoryOps)                      // 原子 + 去重 + 上限 + 扫描
  → manager.Apply(user, userOps)
```

## 7. 边界与已知限制

- **legacy 文件迁移**：聊天者旧的 append 格式文件，首次 `List` 走 legacy 解析为条目，首次 `Apply` 即重写为受管格式——迁移自然发生，无需单独脚本。
- **BLOCKED 条目冻结**：被严格扫描挡住的条目在 `List` 视图里是占位符，模型拿不到原文，故无法对其 replace/remove；该条目保持不变。罕见，接受。
- **超限**：若模型产出的批使序列化结果超过 `MemoryCharLimit` / `UserCharLimit`，`Apply` 拒绝整批。本次提取按失败处理（记 warn + `ResetExtraction` 重置批次，与现有 parse 失败行为一致）。理论上下次仍可能失败，硬化（如回退为仅施加缩减性 ops）留作后续。
- **并发**：`Apply` 经 `MutateAgentFile` 事务执行，节拍写与交互写互斥，安全。
- **成本**：完整记忆进 prompt + 更大输出，比原 200-token 追加更贵；由 `AutoPersist.Model` 选便宜模型与 prompt "从简/压缩" 指示控制。

## 8. 测试计划

- `AutoPersistMemory`（注入构造好的 `*memory.Manager`，可用真引擎 + 内存 store 或 fake）：
  - add / replace / remove 三类 op 正确落盘；
  - 一次调用两组 ops 分别作用于 memory 与 user；
  - 超限批被拒、返回错误（触发批次重置）；
  - JSON 解析失败回退（返回 err）、空 ops no-op；
  - `old_text` 唯一匹配命中、不命中/多命中的处理（复用引擎既有行为）。
- `memory` 工具：
  - schema 不再含 `list`；缺省/空 action 报错；add/replace/remove 正常；
  - 调整/移除原 list 相关用例（`memory_tool_test.go`）。
- `context.go` 渲染：
  - 注入的是消毒条目而非原文（`§`/标记被剥离、威胁条目呈 `[BLOCKED]`）；
  - 仅 `USER.md` / `MEMORY.md` 受影响，其它身份文件注入不变。

## 9. 涉及文件（预估）

- `internal/agent/memory.go` — `AutoPersistMemory` 重写（prompt / schema / Apply / 签名 / max_tokens）。
- `internal/agent/loop.go` — 调用处构造 Manager 并传入。
- `internal/agent/tools/memory_tool.go` — 移除 list 动作。
- `internal/agent/context.go` — 注入改走 manager 渲染。
- 对应测试文件：`memory.go` 新增节拍测试、`memory_tool_test.go`、`context_chatbot_test.go`。

> 测试与产物遵循本机约束：可执行/测试产物不得使用 `claw` 命名或路径；按包跑测试。
