package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/qs3c/bkcrab/internal/provider"
)

// maybeRecoverToolCalls 在模型没有返回原生 tool_calls 但输出了文本时，
// 对响应运行 recoverToolCallsFromContent。成功恢复时会就地修改响应：
// 将解析出的调用拼接进 resp.ToolCalls，用残余文本（XML 之前的可读
// 前言）替换 resp.Content，并清空 RawAssistant，使下一轮的历史
// 重放从已恢复的字段重建助手消息，而不是将错误的 XML 载荷重新
// 回放到模型的上下文中。
//
// 即使没有可恢复的调用，如果 recoverToolCallsFromContent 清理了
// 泄露的特殊 token 噪声（DeepSeek/Qwen 的 `<｜…｜>` 风格分隔符，
// 会被反 token 化为可见的 `<| … |>` / `< | DSML | … >` 垃圾），
// 我们仍会用清理后的版本替换 resp.Content，使 UI 不会渲染泄露的
// 代币。
//
// 在 info 级别记录一次日志，包含 agent + 模型 + 恢复的工具名称，
// 以便运维人员能看到此路径的触发频率以及哪些（模型、提示词）
// 组合触发了它——没有此信号，恢复机制会静默地掩盖本应暴露的
// 真实提示词/工具定义缺陷。
func (a *Agent) maybeRecoverToolCalls(resp *provider.Response) {
	if resp == nil || resp.HasToolCalls() || resp.Content == "" {
		return
	}
	recovered, residual := recoverToolCallsFromContent(resp.Content)
	if len(recovered) == 0 {
		if residual != resp.Content {
			slog.Info("scrubbed_leaked_tool_tokens",
				"agent", a.name, "model", a.model)
			resp.Content = residual
			resp.RawAssistant = nil
		}
		return
	}
	names := make([]string, 0, len(recovered))
	for _, tc := range recovered {
		names = append(names, tc.Function.Name)
	}
	slog.Info("recovered_tool_calls from assistant content",
		"agent", a.name, "model", a.model, "count", len(recovered),
		"tools", names)
	resp.ToolCalls = recovered
	resp.Content = residual
	resp.RawAssistant = nil
}

// recoverToolCallsFromContent 解析某些开源模型（DeepSeek、Qwen 变体）
// 在助手 `content` 字段中以 XML 形式发出的工具调用尝试，而不是使用
// OpenAI Chat Completions 的 `tool_calls` 模式。我们识别的格式是
// 这些模型训练时使用的 Anthropic function_calls XML：
//
// <调用名称=“执行”>
//	  <parameter name="command" string="true">echo hi</parameter>
//	  <parameter name="timeout" string="false">15</parameter>
// </调用>
//
// 返回的工具调用使用合成 ID（`recovered_…`），以便下游的
// tool_result 消息能与原始助手消息配对——模型自身提出的 ID 会
// 与后续轮次的真实 OpenAI 风格 ID 冲突。
//
// 第二个返回值是原始内容去掉所有匹配的 invoke（以及可选的
// <tool_calls>/<function_calls>/<DSML> 包裹块），这样保存的
// 助手消息不会在已恢复的结构化调用旁边保留原始 XML——那样会
// 在聊天 UI 中重复计费工具调用并混淆下一轮。
//
// 当没有 invoke 块匹配时返回 (nil, content)；调用者可以直接
// 跳到正常的"模型未请求工具"分支，恢复路径不会增加任何开销。
func recoverToolCallsFromContent(content string) ([]provider.ToolCall, string) {
	// 快速路径：当内容中完全没有工具调用形状时跳过。
	// 我们同时触发普通的 <invoke 标记和泄露的特殊 token 形状
	// （`<|`、`<｜`、`< | DSML` 等），这样即使没有真实 invoke 可恢复，
	// DeepSeek/Qwen 的反 token 化垃圾至少也能被清理掉。
	if !strings.Contains(content, "<invoke") && !tagLeakHintRE.MatchString(content) {
		return nil, content
	}
// 将泄露的 token 噪声（`<｜tool_calls｜>`、`< | | DSML | |`
		// invoke …>`、关闭变体）折叠为解析器已理解的普通 `<tag …>` 形状。
		// 当内容干净时为空操作。
	normalized := leakedTagRE.ReplaceAllString(content, `<${1}${2}${3}>`)

	matches := invokeRE.FindAllStringSubmatchIndex(normalized, -1)
	if len(matches) == 0 {
		// 没有可恢复的内容。如果规范化改变了内容，我们仍然从残余中
		// 剥离包裹标签，使 UI 不会渲染泄露的 token 垃圾——模型"调用"
		// 了我们无法重建的东西，但至少聊天内容是干净的。
		if normalized == content {
			return nil, content
		}
		scrubbed := strings.TrimSpace(stripRE.ReplaceAllString(normalized, ""))
		return nil, scrubbed
	}
	calls := make([]provider.ToolCall, 0, len(matches))
	for i, m := range matches {
		// m: [整个匹配的起始位置, 结束位置, 名称起始位置, 名称结束位置, 主体起始位置, 主体结束位置]
		name := normalized[m[2]:m[3]]
		body := normalized[m[4]:m[5]]
		args := parseInvokeParameters(body)
		argJSON, err := json.Marshal(args)
		if err != nil {
			// map[string]any 的 JSON 序列化只会在循环时失败——我们的标量
			// map 不可能产生循环——但保留容错行为：跳过此项而不是
			// panic 终止整个轮次。
			continue
		}
		calls = append(calls, provider.ToolCall{
			ID:   fmt.Sprintf("recovered_%d", i),
			Type: "function",
			Function: provider.FunctionCall{
				Name:      name,
				Arguments: string(argJSON),
			},
		})
	}
	if len(calls) == 0 {
		if normalized == content {
			return nil, content
		}
		scrubbed := strings.TrimSpace(stripRE.ReplaceAllString(normalized, ""))
		return nil, scrubbed
	}
	// 将已恢复的 XML 从内容中剥离。我们移除每个 <invoke> 块，
	// 加上常见的外部包裹（tool_calls、function_calls、DSML），
	// 使残余文本只是模型的人类可读前言（如果有的话），没有悬空标签。
	stripped := stripRE.ReplaceAllString(normalized, "")
	stripped = strings.TrimSpace(stripped)
	return calls, stripped
}

// invokeRE 每次提取一个 <invoke name="..."> ... </invoke> 块。
//   - 非贪婪 `(?s).*?` 使相邻的 invoke 不会合并为一个。
//   - 容忍没有 name 属性的 `<invoke>`，通过要求引号限定的 name= 属性；
//     解析器仅用于恢复，而无名 invoke 无论如何无法转为工具调用。
var invokeRE = regexp.MustCompile(`(?s)<invoke\s+name="([^"]+)"\s*>(.*?)</invoke>`)

// parameterRE 匹配 `<parameter name="key" string="true|false">VALUE</parameter>`。
// `string` 属性是类型提示：
//   - string="true"  → VALUE 是 JSON 字符串内容（我们重新引用它）。
//   - string="false" → VALUE 是原始 JSON（数字、布尔、数组、对象）。
//
// 缺少该属性时默认将 VALUE 视为字符串——这是模型省略提示时最安全的
// 解释，也是人类可读 XML 的自然语义。
var parameterRE = regexp.MustCompile(`(?s)<parameter\s+name="([^"]+)"(?:\s+string="(true|false)")?\s*>(.*?)</parameter>`)

// stripRE 匹配：
//   - 每个 invoke 块（成功解析后将其丢弃）
//   - 可选的外部 <tool_calls> / <function_calls> / <DSML>
//     包裹（开标签和闭标签），使残余内容不会保留悬空的 `</tool_calls>`。
var stripRE = regexp.MustCompile(`(?s)<invoke\s+name="[^"]+"\s*>.*?</invoke>|</?(?:tool_calls|function_calls|DSML)\s*/?>`)

// tagLeakHintRE 标记值得运行泄露 token 规范化的内容。
// 我们匹配普通的 `<invoke`（原始恢复触发器）或泄露的特殊 token
// 前缀形状（`<|`、`<｜`、`</|`、`< | …` 等），使不包含可恢复
// <invoke> 的 DeepSeek/Qwen 反 token 化垃圾也能被清理。
var tagLeakHintRE = regexp.MustCompile(`<invoke|<\s*/?\s*[|｜]`)

// leakedTagRE 将泄露的工具调用分隔符折叠回恢复解析器能理解的
// 普通 `<tag …>` / `</tag>` 形状。某些开源模型（DeepSeek-V3/R1、
// Qwen 变体）使用 `<｜tool_calls｜>` / `<｜DSML｜>` 等特殊 token
// 用于工具调用框架；当 tokenizer 往返失败或下游层将这些 token
// 渲染为文本时，用户看到如下形状：
//
// <｜工具调用｜>
// <|工具调用|>
// < | | DSML | |调用名称=“执行”>
// </ | | DSML | |调用>
// <｜/调用｜>
//
// 闭标签的 `/` 可能位于 `<` 正后方或泄露噪声内部（取决于模型
// 将其放在管道的哪一侧），因此我们让任一噪声组消耗它。
//
// 捕获：
//
//	$1 = 闭标签的可选 `/`
//	$2 = 真实标签名（invoke / parameter / tool_calls / function_calls / DSML）
//	$3 = 标签名后的属性（例如 ` name="exec"`）
//
// 替换 `<${1}${2}${3}>` 重构出 `<invoke name="exec">` 等。
// 干净的输入如 `<DSML>` / `<invoke name="x">` 无变化通过，
// 因为所有噪声组都是零宽度的。
var leakedTagRE = regexp.MustCompile(`<(?:[|｜\s]|DSML)*(/?)(?:[|｜\s]|DSML)*(invoke|parameter|tool_calls|function_calls|DSML)([^>]*?)[|｜\s]*>`)

// parseInvokeParameters 遍历一个 invoke 主体内的参数并返回组装的
// 参数映射。未知/格式错误的参数会被静默跳过——我们倾向于用能解析
// 到的参数调用工具，而不是完全拒绝恢复。
func parseInvokeParameters(body string) map[string]any {
	out := map[string]any{}
	for _, p := range parameterRE.FindAllStringSubmatch(body, -1) {
		// p[1]=名称, p[2]="true"/"false"/"", p[3]=原始 VALUE
		name := p[1]
		typeHint := p[2]
		raw := p[3]
		if typeHint == "false" {
			// 原始 JSON 值。如果解析失败，回退为字符串——
			// 发送一个类型错误的参数优于完全丢弃该参数
			// （工具本身通常能强制转换数字字符串，
			// 循环的 BeforeToolCall 钩子会记录参数，
			// 运维人员可以看到通过了什么）。
			var v any
			if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &v); err == nil {
				out[name] = v
				continue
			}
		}
		out[name] = raw
	}
	return out
}
