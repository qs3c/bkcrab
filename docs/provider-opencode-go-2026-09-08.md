# OpenCode Go provider 兼容性排查

日期：2026-09-08。范围：基于 RAG 测评和模型连接测试返回的上游错误，排查 BkCrab 的 provider 请求。

## 结论

DeepSeek V4 Flash / Kimi K3 的 `HTTP 400 MissingSessionID` 已定位为 BkCrab 未适配 OpenCode Go 的会话请求头要求。保持相同账号、模型、端点和请求体，仅增加 `x-opencode-session` 就能将 DeepSeek 从 HTTP 400 变为 HTTP 200。

GPT 5.6 Luna 在补齐请求头后仍返回 HTTP 500，不能用这个修复解释全部模型错误。需要在会话头修复后继续检查该模型的端点/请求契约或上游服务；当前没有足够证据确定 500 的根因。

## 真实请求对照

使用平台已有、已启用的 `opencode-go` 配置。只读取得凭据后在内存使用，不写入报告、终端输出或文件。请求只发送固定 `hi`，每次最多 16 个输出 token；所有对照共用一个新的诊断会话 ID。非流式请求与平台连接测试的请求体一致。

端点：`https://opencode.ai/zen/go/v1/chat/completions`。

| 模型 | 请求差异 | HTTP | 观测 |
| --- | --- | ---: | --- |
| DeepSeek V4 Flash | 原有头；Go 默认 User-Agent | 400 | MissingSessionID |
| DeepSeek V4 Flash | 只加 `User-Agent: BkCrab/diagnostic` | 400 | MissingSessionID |
| DeepSeek V4 Flash | 只加稳定 `x-opencode-session` | 200 | JSON choices / usage |
| DeepSeek V4 Flash | 同时加 User-Agent 和 session | 200 | JSON choices / usage |
| Kimi K3 | 同时加 User-Agent 和 session | 200 | JSON choices / usage |
| GPT 5.6 Luna | 同时加 User-Agent 和 session | 500 | Internal server error |
| DeepSeek V4 Flash | 同时加两头，开启 stream 与 include_usage | 200 | text/event-stream，choices，收到 `[DONE]` |

这证明会话头是本次 DeepSeek 400 的决定因素，且已有 key/端点能够接受请求。16 token 探测的完成 token 可能全部用于 reasoning；HTTP/SSE 通过不等于已验证完整自然语言回答、Planner JSON 或 judge tool call，更不等于 RAG 测评已恢复。

[OpenCode Go 官方文档](https://opencode.ai/docs/go/#where-can-i-use-it)要求客户端使用自己的 User-Agent，并为每段会话发送稳定的 `x-opencode-session`，用于路由及提示缓存。本次实验中仅 session 头已足以解除 DeepSeek 的 400；User-Agent 仍应按官方要求补齐。

## 修复前的本地代码证据

- `internal/provider/openai.go:238` 的 `buildRequest` 为运行时统一构造 OpenAI-compatible 请求；267–268 行仅设置 Content-Type 与 Authorization，没有 session 或自有 User-Agent。
- 同文件 `Chat` 和 `ChatStream` 都调用 `buildRequest`，且两者实际都会请求 SSE，因此流式/非流式 Go 接口均受影响。
- `internal/setup/handlers.go:788` 的 `runProviderTest` 另行构造 HTTP 请求，813–820 行同样缺少这两项；只修运行时会导致“真实调用已好、页面测试仍失败”，只修页面则相反。
- `internal/provider/anthropic.go:297` 的 `buildRequest` 也没有这两项。当前已配置供应商使用 OpenAI-compatible 格式，不能据此声称 Anthropic 协议路径已经实测失败；设计公共请求元数据时应覆盖协议适配器。
- `internal/provider/provider.go` 的 `Provider` 接口没有会话元数据参数；当前也没有 provider 层读取 session context 并转换为上游请求头的机制。

受影响的上层路径：

| 调用 | 路径 | 后果 |
| --- | --- | --- |
| 普通 Agent 对话/工具循环 | `internal/agent/loop.go` → `Provider.Chat/ChatStream` | 相同 OpenCode Go 接入可能直接 400 |
| RAG Planner | `internal/gateway/gateway.go:1416` → `provider.NewProvider` → `Chat` | queryLLM 报错，触发 Planner fallback |
| RAG 回答 | `internal/gateway/gateway.go:1463` resolver → `internal/rag/answer.go:217` | 回答报 provider_error；新测评已有真实错误证据 |
| Ragas 内部 judge 代理 | `internal/setup/handlers_rag_eval_judge.go:87` → `model.Chat` | 若绑定相同供应商，也缺少上游 session 头；本轮在评分之前已取消，未实际验证 judge |
| 模型页面连接测试 | `internal/setup/handlers.go:788` | 页面直接显示连接失败 |

这是代码调用链审计；未在真实普通对话、judge 或 Anthropic 服务上批量发起额外请求。

## 修复应覆盖的范围

1. 在 provider 层建立公共请求元数据处理，使运行时与连接测试共享 User-Agent 和供应商特定请求头逻辑。
2. 上层将逻辑会话标识传入 context。Agent 使用持久会话；知识库聊天使用自身聊天 session；评测的 Planner/回答使用 run + case 的独立作用域；judge 使用可稳定重试的独立评分请求作用域。避免不同用户/会话共享一个全局固定 ID，也避免在每个工具循环或重试时随机更换。
3. 上游会话标识应使用不包含原始用户标识/聊天信息的 opaque 值；同时保证相同逻辑会话多次调用稳定、不同会话隔离。
4. OpenCode 特定头只向目标供应商发送；保持其他供应商的认证和地址语义。连接测试没有持久聊天，可为单次测试生成临时会话作用域。
5. 回归覆盖实际 HTTP 请求、两种 provider 协议、连接测试，以及会话稳定性、隔离、并发、无显式 session 的辅助调用。再做小型 Planner JSON、回答和 judge 工具调用验证，之后恢复四组消融。

仅在一处硬编码全局 session 字符串，或让每次 `buildRequest` 生成新 UUID，都不能完整满足稳定会话要求。

## 已实现的修复与验证

用户授权提交、推送、重新部署后，已实现上述修复范围。`internal/provider/request_metadata.go` 统一设置 `BkCrab/<version>` User-Agent，仅向 `opencode.ai` 添加会话头。逻辑作用域经 SHA-256 转换为 opaque ID，通过 context 传递，不在共享 provider 对象上保存可变会话状态。

Agent 绑定持久 session；RAG chat 绑定 owner/知识库/session；评测 Planner 和回答共享 owner/run/case；judge 使用 owner/规范化评分请求作用域。无持久会话的辅助调用使用临时作用域。OpenAI-compatible、Anthropic 运行时适配器和页面连接测试共用请求头处理。

已通过：

- `go test ./internal/provider ./internal/agent ./internal/rag/... ./internal/setup`。
- `go test -race ./internal/provider`，包含同一个 provider 的并发会话隔离。
- 请求级测试覆盖两种协议的 Chat/ChatStream、连接测试、目标域名限制、会话稳定与隔离；调用链测试覆盖 Agent 多轮、Planner/回答和 judge 重试。

以上属于代码回归验证。部署后的真实连接、RAG 回答及 judge 恢复结果另行记录。保留原 DeepSeek 模型执行实验，不因本次修复更换默认供应商/模型。GPT 5.6 Luna 的上游 500 尚未定位。

本地诊断脚本及脱敏结果：`.tmp/rag-ablation-20260908/probe_provider_headers.py`、`.tmp/rag-ablation-20260908/provider-header-probe.json`。该目录被 git 忽略。
