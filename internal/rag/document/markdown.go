package document

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const internalAssetScheme = "rag-asset://"

var markdownAssetDestinationPattern = regexp.MustCompile(`\]\(\s*(rag-asset://[A-Za-z0-9][A-Za-z0-9._-]{0,255})\s*\)`)

// InternalAssetURL creates the only image URL form trusted parser output may
// contain. Trust still requires ParseInternalAssetURL with the occurrence map
// belonging to the same transient document.
func InternalAssetURL(occurrenceID string) (string, error) {
	if !safeID(occurrenceID) {
		return "", fmt.Errorf("invalid occurrence ID %q", occurrenceID)
	}
	return internalAssetScheme + occurrenceID, nil
}

func ParseInternalAssetURL(raw string, allowInternalAssets bool, occurrences map[string]struct{}) (string, bool) {
	if !allowInternalAssets || !strings.HasPrefix(raw, internalAssetScheme) {
		return "", false
	}
	id := strings.TrimPrefix(raw, internalAssetScheme)
	if !safeID(id) || internalAssetScheme+id != raw {
		return "", false
	}
	_, ok := occurrences[id]
	return id, ok
}

// ValidateMarkdownAssetMarkers verifies typed parser image destinations only.
// Full Markdown/link/HTML normalization belongs to the parse package (Task 7),
// so ordinary text and code containing the same characters are not rewritten
// here.
func ValidateMarkdownAssetMarkers(markdown string, occurrences map[string]struct{}) error {
	var fence byte
	var fenceWidth int
	for _, line := range strings.Split(markdown, "\n") {
		candidate := strings.TrimLeft(line, " ")
		indent := len(line) - len(candidate)
		if indent <= 3 && len(candidate) >= 3 && (candidate[0] == '`' || candidate[0] == '~') {
			width := leadingRun(candidate, candidate[0])
			if width >= 3 {
				if fence == 0 {
					fence, fenceWidth = candidate[0], width
					continue
				}
				if candidate[0] == fence && width >= fenceWidth {
					fence, fenceWidth = 0, 0
					continue
				}
			}
		}
		if fence != 0 || indent >= 4 || strings.HasPrefix(line, "\t") {
			continue
		}
		line = stripInlineCode(line)
		for _, match := range markdownAssetDestinationPattern.FindAllStringSubmatch(line, -1) {
			if len(match) != 2 {
				continue
			}
			if _, ok := ParseInternalAssetURL(match[1], true, occurrences); !ok {
				return fmt.Errorf("internal asset marker %q is not declared by this document", match[1])
			}
		}
	}
	return nil
}

func leadingRun(value string, target byte) int {
	for i := 0; i < len(value); i++ {
		if value[i] != target {
			return i
		}
	}
	return len(value)
}

func stripInlineCode(line string) string {
	var output strings.Builder
	for index := 0; index < len(line); {
		if line[index] != '`' {
			output.WriteByte(line[index])
			index++
			continue
		}
		width := leadingRun(line[index:], '`')
		closer := strings.Index(line[index+width:], strings.Repeat("`", width))
		if closer < 0 {
			output.WriteString(line[index:])
			break
		}
		index += width + closer + width
	}
	return output.String()
}

func FinalCaption(caption, altText, neutral string) string {
	if value := cleanPlainText(caption, 4096); value != "" {
		return value
	}
	if value := cleanPlainText(altText, 4096); value != "" {
		return value
	}
	if value := cleanPlainText(neutral, 4096); value != "" {
		return value
	}
	return "图片（未进行视觉识别）"
}

func cleanPlainText(value string, maxBytes int) string {
	if !validText(value) {
		return ""
	}
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return strings.TrimSpace(value)
}
