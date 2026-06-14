package provider

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
)

// extractLeakedToolCalls 清理某些非 Anthropic 模型
//（特别是通过 xiaomimimo 的 anthropic-compat 端点的 MiMo，
// 以及使用 `<FC6C...>` 全角竖线标记样式的 DeepSeek 衍生模型）
// 以纯文本形式发出的 Claude 风格工具调用 XML，
// 而不是返回类型为 "tool_use" 的结构化 content_block。
// 模型显然见过 Claude 的训练格式
// `<function_calls><invoke name="X"><parameter name="P">v</parameter>
// </invoke></function_calls>`（或类似变体）
// 并按原样复述，但上游网关从未将其转换回 tool_use 块，
// 因此它泄漏到助手的文本内容中。
//
// 检测到时，我们从文本中剥离 XML 并合成代理循环可以正常调度的
// ToolCall 条目。返回清理后的内容和任何合成的调用。
// 如果未找到 XML 模式，则返回未修改的输入文本和 nil 切片。
//
// 标签前缀是可选的且容忍度较高：Claude 使用 `antml:`，
// DeepSeek 风格的模型将标签包裹在全角竖线中，如
// `<FC6C...>`（注意：U+FF5C，不是 ASCII `|`）。
// 外部包装标签是 `function_calls`（Claude）或 `tool_calls`（DSML/OpenAI 风格）。
// 参数上的 `string="true|false"` 属性控制 JSON 解码：
// 当 string="false" 时，值被解析为原始 JSON（数字、布尔值、数组）；
// 否则它被编码为字符串。
const tagPrefixPattern = `(?:antml:|｜｜[^｜<>]+｜｜)?`

var (
	leakedFunctionCallsRe = regexp.MustCompile(`(?s)<` + tagPrefixPattern + `(?:function|tool)_calls>(.*?)</` + tagPrefixPattern + `(?:function|tool)_calls>`)
	leakedInvokeRe        = regexp.MustCompile(`(?s)<` + tagPrefixPattern + `invoke\s+name="([^"]+)"\s*>(.*?)</` + tagPrefixPattern + `invoke>`)
	leakedParameterRe     = regexp.MustCompile(`(?s)<` + tagPrefixPattern + `parameter\s+name="([^"]+)"([^>]*)>(.*?)</` + tagPrefixPattern + `parameter>`)
	leakedStringAttrRe    = regexp.MustCompile(`string="(true|false)"`)
)

func extractLeakedToolCalls(text string) (cleaned string, calls []ToolCall) {
	if text == "" || (!strings.Contains(text, "function_calls") && !strings.Contains(text, "tool_calls")) {
		return text, nil
	}

	matches := leakedFunctionCallsRe.FindAllStringSubmatchIndex(text, -1)
	if len(matches) == 0 {
		return text, nil
	}

	var b strings.Builder
	prev := 0
	for _, m := range matches {
		b.WriteString(text[prev:m[0]])
		prev = m[1]

		body := text[m[2]:m[3]]
		for _, inv := range leakedInvokeRe.FindAllStringSubmatch(body, -1) {
			name := inv[1]
			args := map[string]json.RawMessage{}
			for _, p := range leakedParameterRe.FindAllStringSubmatch(inv[2], -1) {
				pname := p[1]
				attrs := p[2]
				val := p[3]

				asString := true
				if sa := leakedStringAttrRe.FindStringSubmatch(attrs); len(sa) == 2 && sa[1] == "false" {
					asString = false
				}

				if asString {
					raw, _ := json.Marshal(val)
					args[pname] = raw
				} else {
					trimmed := strings.TrimSpace(val)
					if json.Valid([]byte(trimmed)) {
						args[pname] = json.RawMessage(trimmed)
					} else {
						raw, _ := json.Marshal(val)
						args[pname] = raw
					}
				}
			}

			argsJSON, err := json.Marshal(args)
			if err != nil {
				continue
			}
			calls = append(calls, ToolCall{
				ID:   "tooluse_xml_" + randomToolID(),
				Type: "function",
				Function: FunctionCall{
					Name:      name,
					Arguments: string(argsJSON),
				},
			})
		}
	}
	b.WriteString(text[prev:])

	cleaned = strings.TrimSpace(b.String())
	return cleaned, calls
}

func randomToolID() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "0000000000000000"
	}
	return hex.EncodeToString(buf[:])
}
