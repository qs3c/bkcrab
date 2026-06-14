package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/qs3c/bkclaw/internal/toolproviders"
)

// RegisterWebSearchChain 公开了由 a 支持的 web_search 工具
// 工具提供商.链。当链没有配置的提供者时，什么也没有
// 根本没有注册——法学硕士没有看到它不能使用的工具。一种工具，多种
// 提供者，在运行时选择的后备。
func RegisterWebSearchChain(r *Registry, chain *toolproviders.Chain) {
	if chain == nil {
		return
	}
	// “none”是一个哨兵，意味着管理员明确选择退出
	// bkclaw 的 web_search。在链条中的任何位置检测到 → 不检测
	// 完全注册该工具，以便模型恢复到它自己的状态
	// 本机搜索（或根本没有搜索）。
	for _, ref := range chain.Order {
		name := ref
		if i := strings.IndexByte(ref, '/'); i >= 0 {
			name = ref[:i]
		}
		if name == "none" {
			return
		}
	}
	if !chain.Available() {
		return
	}
	r.Register("web_search", "Search the web and return results with titles, URLs, and snippets. Backed by a configurable provider chain (e.g. exa, brave, searxng) with automatic fallback.", map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "The search query",
			},
			"count": map[string]interface{}{
				"type":        "integer",
				"description": "Number of results to return (default 5, max 20)",
			},
		},
		"required": []string{"query"},
	}, func(ctx context.Context, rawArgs json.RawMessage) (string, error) {
		var args map[string]any
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "", fmt.Errorf("parse args: %w", err)
		}
		resp, err := chain.Execute(ctx, args)
		if err != nil {
			return "", err
		}
		return resp.Text, nil
	})
}
