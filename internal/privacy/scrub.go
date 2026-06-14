package privacy

import (
	"regexp"
	"strings"

	"github.com/qs3c/bkclaw/internal/provider"
)

var (
	emailRe      = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	phoneRe      = regexp.MustCompile(`(?:\+\d{1,3}[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}`)
	creditCardRe = regexp.MustCompile(`\b\d{4}[-\s]?\d{4}[-\s]?\d{4}[-\s]?\d{4}\b`)
	ssnRe        = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	ipRe         = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	apiKeyRe     = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_\-]{20,}|AIza[A-Za-z0-9_\-]{30,}|ghp_[A-Za-z0-9]{36,}|AKIA[A-Z0-9]{16}|xoxb-[A-Za-z0-9\-]+)\b`)
	jwtRe        = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\b`)
	privateKeyRe = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
	passwordRe   = regexp.MustCompile(`(?i)("password"\s*:\s*)"[^"]*"`)
)

// Scrub 用占位符替换 PII 模式。
func Scrub(text string) string {
	// 顺序很重要：先处理更长/更具体的模式
	text = privateKeyRe.ReplaceAllString(text, "[PRIVATE_KEY]")
	text = jwtRe.ReplaceAllString(text, "[TOKEN]")
	text = apiKeyRe.ReplaceAllString(text, "[API_KEY]")
	text = creditCardRe.ReplaceAllString(text, "[CARD]")
	text = ssnRe.ReplaceAllString(text, "[SSN]")
	text = emailRe.ReplaceAllString(text, "[EMAIL]")
	text = phoneRe.ReplaceAllString(text, "[PHONE]")
	text = ipRe.ReplaceAllString(text, "[IP]")
	text = passwordRe.ReplaceAllString(text, `${1}"[REDACTED]"`)
	return text
}

// ScrubMessages 从消息内容字段中删除 PII。
func ScrubMessages(messages []provider.Message) []provider.Message {
	out := make([]provider.Message, len(messages))
	for i, m := range messages {
		out[i] = m
		out[i].Content = Scrub(m.Content)
		if len(m.ContentParts) > 0 {
			parts := make([]provider.ContentPart, len(m.ContentParts))
			copy(parts, m.ContentParts)
			for j, p := range parts {
				if p.Type == "text" {
					parts[j].Text = Scrub(p.Text)
				}
			}
			out[i].ContentParts = parts
		}
	}
	return out
}

// ContainsPII 如果文本包含任何可检测的 PII 模式，则返回 true。
func ContainsPII(text string) bool {
	return Scrub(text) != text
}

// 抑制未使用导入的警告。
var _ = strings.TrimSpace
