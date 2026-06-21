package privacy

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ThreatType 分类检测到的内存安全威胁。
type ThreatType string

const (
	ThreatPromptInjection  ThreatType = "prompt_injection"
	ThreatCredentialLeak   ThreatType = "credential_leak"
	ThreatSSHBackdoor      ThreatType = "ssh_backdoor"
	ThreatInvisibleUnicode ThreatType = "invisible_unicode"
	ThreatExfiltration     ThreatType = "exfiltration"
	ThreatPersistenceAbuse ThreatType = "persistence_abuse"
)

// Threat 表示一个检测到的内存安全问题。
type Threat struct {
	Type    ThreatType
	Pattern string
	Context string // snippet of matching text
}

// 提示注入模式（不区分大小写）。
var promptInjectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore\s+previous\s+instructions`),
	regexp.MustCompile(`(?i)disregard\s+all\s+prior`),
	regexp.MustCompile(`(?i)you\s+are\s+now\b`),
	regexp.MustCompile(`(?i)forget\s+everything`),
	regexp.MustCompile(`(?i)new\s+persona`),
	regexp.MustCompile(`(?i)act\s+as\s+[^a-z]`),
}

// 凭据泄漏模式。
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{36,}\b`),
	regexp.MustCompile(`\bxoxb-[A-Za-z0-9\-]+\b`),
	regexp.MustCompile(`\d{18,}\.[A-Za-z0-9_\-]{6,}\.[A-Za-z0-9_\-]{20,}`), // Discord token
}

// SSH 后门模式。
var sshBackdoorPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)authorized_keys`),
	regexp.MustCompile(`(?i)(?:curl|wget)\s+[^\s]+\s*\|\s*(?:bash|sh)`),
}

var strictMemoryPromptInjectionPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)ignore\s+(?:all\s+)?(?:previous|prior)\s+instructions`),
	regexp.MustCompile(`(?i)disregard\s+all\s+prior`),
	regexp.MustCompile(`(?i)\b(?:reveal|output|leak)\s+(?:the\s+)?system\s+prompt\b`),
	regexp.MustCompile(`(?i)developer\s+message`),
	regexp.MustCompile(`(?i)you\s+are\s+now\b`),
	regexp.MustCompile(`(?i)forget\s+everything`),
	regexp.MustCompile(`(?i)new\s+persona`),
	regexp.MustCompile(`(?i)\bact\s+as\s+(?:an?\s+)?[a-z][a-z0-9_-]*`),
	regexp.MustCompile(`(?i)\b(?:remove|disable|bypass)\s+(?:all\s+)?filters?\b`),
}

var strictMemoryExfiltrationPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)output\s+(?:the\s+)?full\s+context`),
	regexp.MustCompile(`(?i)send\s+(?:the\s+)?(?:results?|context|memory|secrets?)\b[^.\n]*(?:https?://|webhook)`),
	regexp.MustCompile(`(?i)(?:(?:context|results?|memory|secrets?|credentials?|tokens?)\b[^.\n]*(?:curl|wget)\s+https?://[^\s]+|(?:curl|wget)\s+https?://[^\s]*(?:context|result|secret|credential|token)[^\s]*)`),
	regexp.MustCompile(`(?i)read\s+(?:/etc/passwd|secret|credential|token)`),
}

var strictMemoryPersistencePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)authorized_keys`),
	regexp.MustCompile(`(?i)\.ssh/authorized_keys\b`),
	regexp.MustCompile(`(?i)(?:curl|wget)\s+[^\s]+\s*\|\s*(?:bash|sh)`),
	regexp.MustCompile(`(?i)(?:modify|edit|overwrite)\s+(?:agent\.json|IDENTITY\.md|SOUL\.md|TOOLS\.md)`),
}

// 要检测的不可见 Unicode 码点。
var invisibleRunes = map[rune]string{
	'\u200B': "ZERO WIDTH SPACE",
	'\u200C': "ZERO WIDTH NON-JOINER",
	'\u200D': "ZERO WIDTH JOINER",
	'\uFEFF': "BOM / ZERO WIDTH NO-BREAK SPACE",
	'\u2060': "WORD JOINER",
	'\u00AD': "SOFT HYPHEN",
}

var strictMemoryInvisibleRunes = map[rune]string{
	'\u202A': "LEFT-TO-RIGHT EMBEDDING",
	'\u202B': "RIGHT-TO-LEFT EMBEDDING",
	'\u202D': "LEFT-TO-RIGHT OVERRIDE",
	'\u202E': "RIGHT-TO-LEFT OVERRIDE",
	'\u2066': "LEFT-TO-RIGHT ISOLATE",
	'\u2067': "RIGHT-TO-LEFT ISOLATE",
	'\u2068': "FIRST STRONG ISOLATE",
	'\u2069': "POP DIRECTIONAL ISOLATE",
}

// Scan 检查文本中的内存安全威胁。
// 返回检测到的威胁列表（空 = 安全）。
func Scan(text string) []Threat {
	var threats []Threat

	// 提示注入
	for _, re := range promptInjectionPatterns {
		if loc := re.FindStringIndex(text); loc != nil {
			threats = append(threats, Threat{
				Type:    ThreatPromptInjection,
				Pattern: re.String(),
				Context: snippet(text, loc[0], loc[1]),
			})
		}
	}

	// 凭据泄漏
	for _, re := range credentialPatterns {
		if loc := re.FindStringIndex(text); loc != nil {
			threats = append(threats, Threat{
				Type:    ThreatCredentialLeak,
				Pattern: re.String(),
				Context: snippet(text, loc[0], loc[1]),
			})
		}
	}

	// SSH 后门
	for _, re := range sshBackdoorPatterns {
		if loc := re.FindStringIndex(text); loc != nil {
			threats = append(threats, Threat{
				Type:    ThreatSSHBackdoor,
				Pattern: re.String(),
				Context: snippet(text, loc[0], loc[1]),
			})
		}
	}

	// 不可见 Unicode
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if name, ok := invisibleRunes[r]; ok {
			threats = append(threats, Threat{
				Type:    ThreatInvisibleUnicode,
				Pattern: name,
				Context: snippet(text, i, i+size),
			})
			break // 检测到一个就足够
		}
		i += size
	}

	return threats
}

func ScanMemoryStrict(text string) []Threat {
	threats := Scan(text)
	appendThreatMatches(&threats, text, ThreatPromptInjection, strictMemoryPromptInjectionPatterns)
	appendThreatMatches(&threats, text, ThreatExfiltration, strictMemoryExfiltrationPatterns)
	appendThreatMatches(&threats, text, ThreatPersistenceAbuse, strictMemoryPersistencePatterns)
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if name, ok := strictMemoryInvisibleRunes[r]; ok {
			threats = append(threats, Threat{
				Type:    ThreatInvisibleUnicode,
				Pattern: name,
				Context: snippet(text, i, i+size),
			})
			break
		}
		i += size
	}
	return dedupeThreats(threats)
}

func appendThreatMatches(threats *[]Threat, text string, threatType ThreatType, patterns []*regexp.Regexp) {
	for _, re := range patterns {
		if loc := re.FindStringIndex(text); loc != nil {
			*threats = append(*threats, Threat{
				Type:    threatType,
				Pattern: re.String(),
				Context: snippet(text, loc[0], loc[1]),
			})
		}
	}
}

func dedupeThreats(threats []Threat) []Threat {
	type dedupeKey struct {
		Type    ThreatType
		Context string
	}
	seen := make(map[dedupeKey]struct{}, len(threats))
	deduped := threats[:0]
	for _, threat := range threats {
		key := dedupeKey{Type: threat.Type, Context: threat.Context}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, threat)
	}
	return deduped
}

// snippet 提取匹配位置周围的上下文片段。
func snippet(text string, start, end int) string {
	const pad = 40
	lo := start - pad
	if lo < 0 {
		lo = 0
	}
	hi := end + pad
	if hi > len(text) {
		hi = len(text)
	}
	s := text[lo:hi]
	s = strings.ReplaceAll(s, "\n", " ")
	if lo > 0 {
		s = "..." + s
	}
	if hi < len(text) {
		s = s + "..."
	}
	return s
}
