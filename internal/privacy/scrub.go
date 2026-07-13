package privacy

import (
	"regexp"
	"strings"

	"github.com/qs3c/bkcrab/internal/provider"
)

var (
	emailRe                 = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	phoneRe                 = regexp.MustCompile(`(?:\+\d{1,3}[-.\s]?)?\(?\d{3}\)?[-.\s]?\d{3}[-.\s]?\d{4}`)
	creditCardRe            = regexp.MustCompile(`\b\d{4}[-\s]?\d{4}[-\s]?\d{4}[-\s]?\d{4}\b`)
	ssnRe                   = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	ipRe                    = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}\b`)
	apiKeyRe                = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_\-]{20,}|AIza[A-Za-z0-9_\-]{30,}|ghp_[A-Za-z0-9]{36,}|AKIA[A-Z0-9]{16}|xoxb-[A-Za-z0-9\-]+)\b`)
	jwtRe                   = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\b`)
	privateKeyRe            = regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`)
	passwordRe              = regexp.MustCompile(`(?i)("password"\s*:\s*)"[^"]*"`)
	labeledSensitiveValueRe = regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?key|token|secret|password|credential|account[_-]?id|tenant[_-]?id|project[_-]?id|customer[_-]?id)\b\s*[:=]\s*["']?([^\s"',;]+)`)
	identityValueRe         = regexp.MustCompile(`(?i)\b(?:customer[_-]?name|employee[_-]?name|tenant[_-]?name|account[_-]?name)\b\s*[:=]\s*["']?([^\n"',;]+)`)
	credentialURLRe         = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]+:[^@\s/]+@`)
	placeholderRe           = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
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

// ContainsSensitiveInstanceData extends the PII formats with labeled
// credentials/tenant values commonly copied from tool arguments into a
// generated learner skill. Placeholders and environment-variable references
// remain valid because shared skills are expected to parameterize them.
func ContainsSensitiveInstanceData(text string) bool {
	if ContainsPII(text) || credentialURLRe.MatchString(text) {
		return true
	}
	for _, re := range []*regexp.Regexp{labeledSensitiveValueRe, identityValueRe} {
		for _, match := range re.FindAllStringSubmatch(text, -1) {
			if len(match) > 1 && !isConfigurationPlaceholder(match[1]) {
				return true
			}
		}
	}
	return false
}

func isConfigurationPlaceholder(value string) bool {
	value = strings.TrimSpace(strings.Trim(value, `"'`))
	if value == "" {
		return true
	}
	if strings.HasPrefix(value, "$") || strings.HasPrefix(value, "{") ||
		strings.HasPrefix(value, "<") || strings.HasPrefix(value, "[") {
		return true
	}
	if placeholderRe.MatchString(value) {
		return true
	}
	lower := strings.ToLower(value)
	for _, marker := range []string{"placeholder", "example", "redacted", "your_", "your-", "{user}", "{project}"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// 抑制未使用导入的警告。
var _ = strings.TrimSpace
