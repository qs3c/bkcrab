package provider

import (
	"net/url"
	"strings"
)

// NormalizeAPIBase 将用户输入的 apiBase 转换为下游代码期望的标准形式，
// 以便在其上拼接路径段。
//
// 不同的 API 类型对 `/v1` 是属于基础 URL 还是路径的一部分有不同看法：
//
//   - OpenAI Chat Completions: 运行时追加 "/chat/completions"，
//     假设 /v1 已在基础 URL 中。裸主机返回 404。
//   - Anthropic Messages: 运行时追加 "/v1/messages"，假设 /v1
//     不在基础 URL 中。尾部有 /v1 会产生 /v1/v1/messages。
//
// 两种形式都是常见的拼写错误（人们从文档页面复制 "https://api.openai.com"，
// 或者类比 OpenAI 粘贴 "https://api.anthropic.com/v1"）。
// 我们在这里将它们统一为标准形式，以便连接测试、运行时和任何其他消费者
// 都访问相同的 URL。
//
// 规则有意保持保守——我们只处理尾部的 /v1 段，且仅在用户提供裸主机
//（无自定义路径）时进行。第三方网关有自己的路由约定
//（例如 "https://my-gateway.com/openai"）保持不变，因为我们
// 无法安全地猜测 /v1 在其路径中的位置。
func NormalizeAPIBase(apiBase, apiType string) string {
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		return ""
	}
	switch apiType {
	case "anthropic-messages":
		return strings.TrimSuffix(base, "/v1")
	default:
		u, err := url.Parse(base)
		if err != nil || u.Path != "" {
			return base
		}
		return base + "/v1"
	}
}
