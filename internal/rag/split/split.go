// Package split turns parsed document text into searchable chunks.
package split

import (
	"strings"
	"unicode"
)

// Config controls the approximate size of generated chunks.
type Config struct {
	ChunkSize    int // target token count; defaults to 512
	ChunkOverlap int // approximate adjacent overlap; defaults to 64
}

func (c *Config) normalize() {
	if c.ChunkSize <= 0 {
		c.ChunkSize = 512
	}
	if c.ChunkOverlap < 0 || c.ChunkOverlap >= c.ChunkSize {
		c.ChunkOverlap = c.ChunkSize / 8
	}
}

// Chunk is a searchable piece of a document.
type Chunk struct {
	Index        int
	Content      string
	SectionTitle string
	PageNum      int
	Tokens       int
}

// EstimateTokens counts each CJK rune as one token and every four other runes
// as one token, rounded up.
func EstimateTokens(s string) int {
	cjk, other := 0, 0
	for _, r := range s {
		if isCJK(r) {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

func isCJK(r rune) bool {
	return unicode.Is(unicode.Han, r) ||
		unicode.Is(unicode.Hiragana, r) ||
		unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r)
}

// splitSentences preserves sentence terminators in the returned pieces.
func splitSentences(text string) []string {
	var out []string
	var current strings.Builder
	for _, r := range text {
		current.WriteRune(r)
		switch r {
		case '。', '！', '？', '；', '.', '!', '?', ';', '\n':
			if piece := current.String(); strings.TrimSpace(piece) != "" {
				out = append(out, piece)
			}
			current.Reset()
		}
	}
	if piece := current.String(); strings.TrimSpace(piece) != "" {
		out = append(out, piece)
	}
	return out
}

// splitToFit breaks an oversized sentence at rune boundaries.
func splitToFit(text string, maxTokens int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if EstimateTokens(text) <= maxTokens {
		return []string{text}
	}
	runes := []rune(text)
	start, cjk, other := 0, 0, 0
	pieces := make([]string, 0, EstimateTokens(text)/maxTokens+1)
	for i, r := range runes {
		nextCJK, nextOther := cjk, other
		if isCJK(r) {
			nextCJK++
		} else {
			nextOther++
		}
		if nextCJK+(nextOther+3)/4 > maxTokens && i > start {
			pieces = append(pieces, string(runes[start:i]))
			start = i
			if isCJK(r) {
				cjk, other = 1, 0
			} else {
				cjk, other = 0, 1
			}
			continue
		}
		cjk, other = nextCJK, nextOther
	}
	if start < len(runes) {
		pieces = append(pieces, string(runes[start:]))
	}
	return pieces
}

// suffixForTokens returns the longest suffix within maxTokens.
func suffixForTokens(text string, maxTokens int) string {
	if maxTokens <= 0 || text == "" {
		return ""
	}
	runes := []rune(text)
	start, cjk, other := len(runes), 0, 0
	for i := len(runes) - 1; i >= 0; i-- {
		nextCJK, nextOther := cjk, other
		if isCJK(runes[i]) {
			nextCJK++
		} else {
			nextOther++
		}
		if nextCJK+(nextOther+3)/4 > maxTokens {
			break
		}
		cjk, other, start = nextCJK, nextOther, i
	}
	return string(runes[start:])
}

// SlidingWindow greedily packs sentences up to ChunkSize and retains up to
// ChunkOverlap estimated tokens between adjacent chunks.
func SlidingWindow(text string, cfg Config, sectionTitle string, pageNum int) []Chunk {
	cfg.normalize()
	var pieces []string
	for _, sentence := range splitSentences(text) {
		pieces = append(pieces, splitToFit(sentence, cfg.ChunkSize)...)
	}
	var chunks []Chunk
	buffer := ""
	emit := func(text string) {
		content := strings.TrimSpace(text)
		if content == "" {
			return
		}
		chunks = append(chunks, Chunk{
			Index: len(chunks), Content: content, SectionTitle: sectionTitle,
			PageNum: pageNum, Tokens: EstimateTokens(content),
		})
	}
	for _, piece := range pieces {
		if buffer != "" && EstimateTokens(buffer+piece) > cfg.ChunkSize {
			emit(buffer)
			buffer = suffixForTokens(buffer, cfg.ChunkOverlap)
			remaining := cfg.ChunkSize - EstimateTokens(piece)
			if EstimateTokens(buffer) > remaining {
				buffer = suffixForTokens(buffer, remaining)
			}
		}
		if EstimateTokens(buffer+piece) > cfg.ChunkSize {
			buffer = ""
		}
		buffer += piece
	}
	emit(buffer)
	return chunks
}

type section struct {
	title string
	body  strings.Builder
}

// Markdown splits ATX-heading sections, then applies a window within each one.
func Markdown(markdown string, cfg Config) []Chunk {
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	markdown = strings.ReplaceAll(markdown, "\r", "\n")
	sections := []*section{{}}
	current := sections[0]
	var titles [6]string
	for _, line := range strings.Split(markdown, "\n") {
		if level, title, ok := atxHeading(line); ok {
			titles[level-1] = title
			for i := level; i < len(titles); i++ {
				titles[i] = ""
			}
			breadcrumb := make([]string, 0, level)
			for i := 0; i < level; i++ {
				if titles[i] != "" {
					breadcrumb = append(breadcrumb, titles[i])
				}
			}
			current = &section{title: strings.Join(breadcrumb, " > ")}
			sections = append(sections, current)
			continue
		}
		current.body.WriteString(line)
		current.body.WriteByte('\n')
	}
	var chunks []Chunk
	for _, sec := range sections {
		body := strings.TrimSpace(sec.body.String())
		if body == "" {
			continue
		}
		for _, chunk := range SlidingWindow(body, cfg, sec.title, 0) {
			chunk.Index = len(chunks)
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}

func atxHeading(line string) (level int, title string, ok bool) {
	trimmed := strings.TrimSpace(line)
	for level < len(trimmed) && level < 6 && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level >= len(trimmed) ||
		(trimmed[level] != ' ' && trimmed[level] != '\t') {
		return 0, "", false
	}
	title = strings.TrimSpace(trimmed[level:])
	title = strings.TrimSpace(strings.TrimRight(title, "#"))
	if title == "" {
		return 0, "", false
	}
	return level, title, true
}

// Pages chunks pages independently and preserves one-based page numbers.
func Pages(pages []string, cfg Config) []Chunk {
	var chunks []Chunk
	for i, page := range pages {
		for _, chunk := range SlidingWindow(page, cfg, "", i+1) {
			chunk.Index = len(chunks)
			chunks = append(chunks, chunk)
		}
	}
	return chunks
}
