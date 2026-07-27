package split

import (
	"strings"
	"unicode/utf8"
)

type codeData struct {
	content  string
	language string
}

func serializeCode(content, language string) string {
	content = normalizeNewlines(content)
	fence := strings.Repeat("`", max(3, longestRun(content, '`')+1))
	var output strings.Builder
	output.WriteString(fence)
	if language != "" {
		output.WriteString(language)
	}
	output.WriteByte('\n')
	output.WriteString(content)
	if content != "" && !strings.HasSuffix(content, "\n") {
		output.WriteByte('\n')
	}
	output.WriteString(fence)
	return output.String()
}

func splitCode(block semanticBlock, cfg Config) []semanticBlock {
	if block.code == nil {
		return splitAtomicText(block, cfg)
	}
	reserve := cfg.enhancementReserve()
	if fitsBlock(block, renderCodeBlock(block, block.code.content), cfg, reserve) {
		clone := block
		clone.raw = renderCodeBlock(block, block.code.content)
		return []semanticBlock{clone}
	}

	groups := codeLogicalGroups(block.code.content)
	var result []semanticBlock
	buffer := ""
	emit := func() {
		if buffer == "" {
			return
		}
		clone := block
		clone.raw = renderCodeBlock(block, buffer)
		result = append(result, clone)
		buffer = ""
	}

	for _, group := range groups {
		candidate := buffer + group
		if candidate != "" && fitsBlock(block, renderCodeBlock(block, candidate), cfg, reserve) {
			buffer = candidate
			continue
		}
		emit()
		if fitsBlock(block, renderCodeBlock(block, group), cfg, reserve) {
			buffer = group
			continue
		}
		for _, line := range splitCodeLines(group) {
			candidate = buffer + line
			if candidate != "" && fitsBlock(block, renderCodeBlock(block, candidate), cfg, reserve) {
				buffer = candidate
				continue
			}
			emit()
			if fitsBlock(block, renderCodeBlock(block, line), cfg, reserve) {
				buffer = line
				continue
			}
			for _, piece := range splitOversizedCodeLine(block, line, cfg) {
				clone := block
				clone.raw = renderCodeBlock(block, piece)
				result = append(result, clone)
			}
		}
	}
	emit()
	if len(result) == 0 {
		clone := block
		clone.raw = renderCodeBlock(block, block.code.content)
		result = append(result, clone)
	}
	return result
}

func renderCodeBlock(block semanticBlock, content string) string {
	language := ""
	if block.code != nil {
		language = block.code.language
	}
	return wrapSemanticRaw(block, serializeCode(content, language))
}

// codeLogicalGroups prefers function declarations and blank lines before the
// later line/rune fallback. Lines retain their original newline bytes.
func codeLogicalGroups(content string) []string {
	lines := splitCodeLines(content)
	if len(lines) == 0 {
		return nil
	}
	var groups []string
	var current strings.Builder
	previousBlank := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		startsFunction := looksLikeFunctionBoundary(trimmed)
		if current.Len() > 0 && (startsFunction || previousBlank) && trimmed != "" {
			groups = append(groups, current.String())
			current.Reset()
		}
		current.WriteString(line)
		previousBlank = trimmed == ""
	}
	if current.Len() > 0 {
		groups = append(groups, current.String())
	}
	return groups
}

func splitCodeLines(content string) []string {
	if content == "" {
		return nil
	}
	content = normalizeNewlines(content)
	lines := strings.SplitAfter(content, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

func looksLikeFunctionBoundary(line string) bool {
	prefixes := []string{
		"func ", "def ", "class ", "function ", "async function ",
		"export function ", "export async function ", "type ", "interface ",
		"public ", "private ", "protected ", "fn ",
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func splitOversizedCodeLine(block semanticBlock, line string, cfg Config) []string {
	reserve := cfg.enhancementReserve()
	remaining := line
	var result []string
	continuation := false
	for remaining != "" {
		marker := ""
		if continuation {
			marker = "（续）"
		}
		prefix := longestPrefixThatFits(remaining, func(candidate string) bool {
			return fitsBlock(block, renderCodeBlock(block, marker+candidate), cfg, reserve)
		})
		if prefix == "" && marker != "" {
			marker = ""
			prefix = longestPrefixThatFits(remaining, func(candidate string) bool {
				return fitsBlock(block, renderCodeBlock(block, candidate), cfg, reserve)
			})
		}
		if prefix == "" {
			_, size := utf8.DecodeRuneInString(remaining)
			prefix = remaining[:size]
		}
		result = append(result, marker+prefix)
		remaining = remaining[len(prefix):]
		continuation = true
	}
	return result
}
