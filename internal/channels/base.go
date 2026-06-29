package channels

import (
	"context"
	"strings"

	"github.com/qs3c/bkcrab/internal/bus"
)

// SplitMessageMarker 是 LLM 在线路上发出的控制标记，用于请求 IM 风格
// 适配器（微信等）将单个出站文本负载拆分为多个独立聊天气泡。我们选择了一个
// 不会出现在自然散文中的标记，这样 agent 不会在 markdown / 代码 / 引用文本
// 中意外触发拆分；
//
//  1. 它能通过微信的 wechatStripMarkdown 处理——不会被解析为任何 markdown 构造；
//  2. 对检查记录的人类和发出它的 LLM 来说都读作"控制指令"。
//
// 引入此标记的 agent 侧提示位于 internal/agent/loop.go 的每轮系统提示附录中，
// 因此协议只在唯一的地点声明。
const SplitMessageMarker = "<|split|>"

// FlattenMarkdownTables 将 `text` 中的每个 GFM 风格表格块转换为
// IM 渠道实际渲染的平面无语法形式。我们支持的所有 IM 平台
//（Discord、Telegram、LINE、Slack、飞书、微信）都不渲染 markdown
// 表格——它们将表格作为原始 `|cell|cell|` 行加上 `|---|---|` 分隔线
// 直接显示，这对话者来说看起来像故障。
//
// 检测是 GFM 严格的：表格是两行或更多连续行，其中第一行是标题行，
// 第二行是分隔行（`|---|...`，可选对齐冒号），之后的所有内容是数据行
// 直到遇到非表格行。任何不匹配的内容逐字节原样通过——包含管道的
// 引用文本、代码围栏、散文中偶然出现的 "|" 都能存活。
//
// 输出形状：
//
//	2 列表格  → "header1: header2" 行，然后每行一个 "cell1: cell2" 行。
//	              这是 LLM 发出的最常见形状（标签/值列表），作为纯文本
//	              可清晰阅读。
//	3+ 列表格 → 单元格用 " · "（中点）连接，无分隔行。丢弃对齐但每行
//	              保持一行且一目了然可扫描为表格。
//
// 单元格被修剪；GFM 转义 `\|` 在单元格内回退为字面 `|`。分隔行
// 在任何形状下都被丢弃。
func FlattenMarkdownTables(text string) string {
	if !strings.Contains(text, "|") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		// 当我们看到一行后面紧跟分隔行时，表格开始。其它情况——
		// 即使是偶然包含管道的行——原样通过。
		if i+1 < len(lines) && isMarkdownTableRow(lines[i]) && isMarkdownTableSeparator(lines[i+1]) {
			header := parseMarkdownTableRow(lines[i])
			i += 2 // consume header + separator
			rows := [][]string{header}
			for i < len(lines) && isMarkdownTableRow(lines[i]) {
				rows = append(rows, parseMarkdownTableRow(lines[i]))
				i++
			}
			out = append(out, renderFlatTable(rows))
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n")
}

// isMarkdownTableRow 在修剪后的行以非转义 `|` 开头和结尾且中间
// 包含至少一个 `|` 时返回 true——即 GFM 表格行形状。单独的 "|"
// 或单字段不算；否则散文行上的误报成本太高。
func isMarkdownTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 || trimmed[0] != '|' || trimmed[len(trimmed)-1] != '|' {
		return false
	}
	// 需要内部管道——计算边界内的非转义管道。
	interior := trimmed[1 : len(trimmed)-1]
	for i := 0; i < len(interior); i++ {
		if interior[i] == '|' && (i == 0 || interior[i-1] != '\\') {
			return true
		}
	}
	return false
}

// isMarkdownTableSeparator 对 GFM 表格分隔行返回 true——每行中
// 每个单元格匹配 `^\s*:?-+:?\s*$` 的行。也容忍空管道行，原因与
// GFM 相同（某些发送器在空列中跳过短划线）。
func isMarkdownTableSeparator(line string) bool {
	if !isMarkdownTableRow(line) {
		return false
	}
	cells := parseMarkdownTableRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		c = strings.TrimPrefix(c, ":")
		c = strings.TrimSuffix(c, ":")
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' {
				return false
			}
		}
	}
	return true
}

// parseMarkdownTableRow 将一个表格行拆分为修剪后的单元格。遵循
// GFM 的 `\|` 转义，使包含字面管道的单元格可以往返。
func parseMarkdownTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	// 丢弃前导/尾部管道；否则拆分会在两端产生幽灵空单元格。
	if strings.HasPrefix(trimmed, "|") {
		trimmed = trimmed[1:]
	}
	if strings.HasSuffix(trimmed, "|") {
		trimmed = trimmed[:len(trimmed)-1]
	}
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c == '\\' && i+1 < len(trimmed) && trimmed[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if c == '|' {
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	cells = append(cells, strings.TrimSpace(cur.String()))
	return cells
}

// renderFlatTable 将解析后的行格式化为纯文本渠道文本。rows[0] 是标题。
// 形状规则参见 FlattenMarkdownTables。
func renderFlatTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	var b strings.Builder
	if cols == 2 {
		for i, r := range rows {
			left := ""
			right := ""
			if len(r) > 0 {
				left = r[0]
			}
			if len(r) > 1 {
				right = r[1]
			}
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(left)
			b.WriteString(": ")
			b.WriteString(right)
		}
		return b.String()
	}
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		// 填充短行以保持列对齐可读；缺失单元格在点之间显示为空字符串。
		padded := r
		for len(padded) < cols {
			padded = append(padded, "")
		}
		b.WriteString(strings.Join(padded, " · "))
	}
	return b.String()
}

// SplitOutboundText 在 SplitMessageMarker 处将回复负载拆分为适配器应
// 发送的每个气泡一个块。修剪每个块上的空白并丢弃空块，以避免尾部标记
// 或意外双重拆分产生空白消息。在 agent 未请求拆分的常见情况下返回
// 单元素切片——适配器可以无条件调用而无需分支。
func SplitOutboundText(text string) []string {
	parts := strings.Split(text, SplitMessageMarker)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Channel 是所有渠道实现必须满足的接口。
type Channel interface {
	// Name 返回渠道类型标识符（例如 "telegram"）。
	Name() string
	// AccountID 返回渠道内的账号标识符。
	AccountID() string
	// BotUsername 返回此渠道的机器人用户名（例如 "mike_bkcrab_bot"）。
	// 不适用时返回空字符串。
	BotUsername() string
	// Start 开始监听消息。应阻塞直到 ctx 被取消。
	Start(ctx context.Context) error
	// Send 向指定聊天发送纯文本消息。
	Send(chatID string, text string) error
	// SendMessage 发送带格式、回复、按钮等功能的富出站消息。
	SendMessage(msg bus.OutboundMessage) error
	// SendTyping 向指定聊天发送输入指示器。
	SendTyping(chatID string) error
}
