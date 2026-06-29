// Package webfetch 包含内置的 web_fetch 提供商。每个提供商接受相同的
// {url, max_length} 参数格式，并返回 LLM 可以直接阅读的纯文本。
// 每次调用的凭据/端点来自 toolproviders.Request.Config，因此提供商保持无状态。
package webfetch

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/qs3c/bkcrab/internal/toolproviders"
)

// Category 是这些提供商插入的工具类别。
const Category = "web_fetch"

// DefaultMaxLen 是调用方未传递 max_length 时的上限。
// 与旧版纯 direct web_fetch 工具使用的值相同，以便切换到链时不会改变回复截断行为。
const DefaultMaxLen = 10000

// RegisterAll 在 r 中注册所有内置的 web_fetch 提供商。
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&Direct{})
	r.Register(&Jina{})
	r.Register(&Firecrawl{})
}

type args struct {
	URL    string
	MaxLen int
}

func parseArgs(raw map[string]any) (args, error) {
	var a args
	if s, ok := raw["url"].(string); ok {
		a.URL = strings.TrimSpace(s)
	}
	if a.URL == "" {
		return a, fmt.Errorf("url is required")
	}
	switch v := raw["max_length"].(type) {
	case float64:
		a.MaxLen = int(v)
	case int:
		a.MaxLen = v
	}
	if a.MaxLen <= 0 {
		a.MaxLen = DefaultMaxLen
	}
	return a, nil
}

// truncate 在 maxLen 处截断文本并添加可见标记，以便 LLM 知道页面比收到的内容更长，
// 可以请求更高的上限（或选择更具体的 URL），而不是将截断视为完整内容。
func truncate(text string, maxLen int) string {
	if maxLen <= 0 || len(text) <= maxLen {
		return text
	}
	return text[:maxLen] + "\n[...truncated]"
}

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// stripHTML 移除 script/style 块、删除剩余的 HTML 标签并折叠空白。
// 与原来在代理的 web_fetch 工具中的辅助函数相同，
// 以便 Direct 提供商产生相同的输出。
func stripHTML(html string) string {
	scriptRe := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	html = scriptRe.ReplaceAllString(html, "")
	styleRe := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	html = styleRe.ReplaceAllString(html, "")

	text := htmlTagRe.ReplaceAllString(html, " ")

	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")

	spaceRe := regexp.MustCompile(`[ \t]+`)
	text = spaceRe.ReplaceAllString(text, " ")
	nlRe := regexp.MustCompile(`\n{3,}`)
	text = nlRe.ReplaceAllString(text, "\n\n")

	return strings.TrimSpace(text)
}
