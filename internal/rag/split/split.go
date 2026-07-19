// Package split turns parsed document text into searchable chunks.
package split

import (
	"strings"
	"unicode"

	"github.com/qs3c/bkcrab/internal/rag/chunktext"
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
	Index         int
	Content       string
	SearchContent string
	SectionTitle  string
	PageNum       int
	Tokens        int
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
			Index: len(chunks), Content: content, SearchContent: content,
			SectionTitle: sectionTitle,
			PageNum:      pageNum, Tokens: EstimateTokens(content),
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

type markdownFence struct {
	marker byte
	length int
}

// fenceOpening recognizes CommonMark-style backtick and tilde fences with up
// to three leading spaces. The caller tracks the opening marker so headings in
// fenced code are never interpreted as document structure.
func fenceOpening(line string) (markdownFence, bool) {
	leading := len(line) - len(strings.TrimLeft(line, " "))
	if leading > 3 {
		return markdownFence{}, false
	}
	trimmed := line[leading:]
	if len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return markdownFence{}, false
	}
	marker := trimmed[0]
	length := 0
	for length < len(trimmed) && trimmed[length] == marker {
		length++
	}
	if length < 3 {
		return markdownFence{}, false
	}
	// A backtick fence's info string cannot itself contain a backtick.
	if marker == '`' && strings.Contains(trimmed[length:], "`") {
		return markdownFence{}, false
	}
	return markdownFence{marker: marker, length: length}, true
}

func (f markdownFence) closes(line string) bool {
	leading := len(line) - len(strings.TrimLeft(line, " "))
	if leading > 3 {
		return false
	}
	trimmed := line[leading:]
	length := 0
	for length < len(trimmed) && trimmed[length] == f.marker {
		length++
	}
	return length >= f.length && strings.TrimSpace(trimmed[length:]) == ""
}

// sectionTitleForSearch bounds repeated heading context to one quarter of the
// configured chunk size. When truncation is necessary, retain the breadcrumb's
// most specific suffix; SectionTitle itself always keeps the complete value.
func sectionTitleForSearch(title string, chunkSize int) string {
	if title == "" {
		return ""
	}
	for budget := max(1, chunkSize/4); budget > 0; budget-- {
		candidate := title
		if EstimateTokens(candidate) > budget {
			ellipsis := "…"
			remaining := budget - EstimateTokens(ellipsis)
			candidate = ellipsis
			if remaining > 0 {
				candidate += suffixForTokens(title, remaining)
			}
		}
		// Always leave room for at least one body token. Extremely small chunk
		// sizes may therefore omit the repeated title while preserving it in
		// SectionTitle metadata.
		if EstimateTokens(chunktext.Search(candidate, "")) < chunkSize {
			return candidate
		}
	}
	return ""
}

// Markdown splits ATX-heading sections, then applies a window within each one.
func Markdown(markdown string, cfg Config) []Chunk {
	cfg.normalize()
	markdown = strings.ReplaceAll(markdown, "\r\n", "\n")
	markdown = strings.ReplaceAll(markdown, "\r", "\n")
	sections := []*section{{}}
	current := sections[0]
	var titles [6]string
	var fence markdownFence
	inFence := false
	for _, line := range strings.Split(markdown, "\n") {
		if inFence {
			current.body.WriteString(line)
			current.body.WriteByte('\n')
			if fence.closes(line) {
				inFence = false
			}
			continue
		}
		if opening, ok := fenceOpening(line); ok {
			fence = opening
			inFence = true
			current.body.WriteString(line)
			current.body.WriteByte('\n')
			continue
		}
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
		searchTitle := sectionTitleForSearch(sec.title, cfg.ChunkSize)
		bodyCfg := cfg
		if searchTitle != "" {
			prefixTokens := EstimateTokens(chunktext.Search(searchTitle, ""))
			bodyCfg.ChunkSize = max(1, cfg.ChunkSize-prefixTokens)
			if bodyCfg.ChunkOverlap >= bodyCfg.ChunkSize {
				bodyCfg.ChunkOverlap = min(cfg.ChunkOverlap, bodyCfg.ChunkSize/8)
			}
		}
		for _, chunk := range SlidingWindow(body, bodyCfg, sec.title, 0) {
			chunk.SearchContent = chunktext.Search(searchTitle, chunk.Content)
			chunk.Tokens = EstimateTokens(chunk.SearchContent)
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
