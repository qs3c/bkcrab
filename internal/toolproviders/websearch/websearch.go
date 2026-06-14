// Package websearch 包含内置的 web_search 提供商。每个都是纯 Go 结构体——
// 没有子进程 IPC——因此多租户调用就是普通的函数调用。
// 每次请求的凭据来自 toolproviders.Request.Config，因此提供商不持有租户状态。
package websearch

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// Category 是这些提供商插入的工具类别。
const Category = "web_search"

// RegisterAll 将所有内置的 web_search 提供商添加到 r。
// 提供商无条件自我注册；某个提供商在运行时是否真正被使用，
// 取决于 Chain 的 GetConfig 是否返回可用的密钥/端点。
func RegisterAll(r *toolproviders.Registry) {
	r.Register(&Brave{})
	r.Register(&Exa{})
	r.Register(&SearxNG{})
	r.Register(&None{})
}

// --- 共享参数解析 ---

type args struct {
	Query string
	Count int
}

func parseArgs(raw map[string]any) (args, error) {
	var out args
	if q, ok := raw["query"].(string); ok {
		out.Query = strings.TrimSpace(q)
	}
	if out.Query == "" {
		return out, fmt.Errorf("query is required")
	}
	// JSON 数字通过 map[string]any 解码为 float64。
	switch v := raw["count"].(type) {
	case float64:
		out.Count = int(v)
	case int:
		out.Count = v
	}
	if out.Count <= 0 {
		out.Count = 5
	}
	if out.Count > 20 {
		out.Count = 20
	}
	return out, nil
}

// resultItem 是每个提供商在渲染前规范化的内部结构。
// 确保无论后端如何，LLM 可见的输出保持一致。
type resultItem struct {
	Title   string
	URL     string
	Snippet string
}

// snippetTagRe 匹配某些搜索后端（特别是通过 SearXNG 的百度/搜狗）
// 泄漏到 `content` 字段中的 HTML 标签——通常是围绕匹配查询词的
// <strong>…</strong> 包裹。它们对模型来说看起来像"脏数据"，
// 并触发"让我获取真实文章来验证"的反射行为，即使摘要本身已经回答了问题。
// 提前剥离它们使摘要看起来像权威的纯文本。
var snippetTagRe = regexp.MustCompile(`<[^>]+>`)

func cleanSnippet(s string) string {
	s = snippetTagRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", "\"")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	// 折叠移除标签后留下的多空格产物。
	s = regexp.MustCompile(`[ \t]+`).ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func render(query string, items []resultItem) string {
	if len(items) == 0 {
		return "No results found for: " + query
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Search results for: %s\n\n", query)
	for i, it := range items {
		fmt.Fprintf(&sb, "%d. %s\n   URL: %s\n   %s\n\n", i+1, it.Title, it.URL, cleanSnippet(it.Snippet))
	}
	return sb.String()
}
