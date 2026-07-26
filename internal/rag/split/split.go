// Package split turns canonical Markdown artifacts into deterministic,
// block-aware searchable chunks. It never performs network I/O.
package split

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/rag/chunktext"
	"github.com/qs3c/bkcrab/internal/rag/document"
)

// BlockKind identifies the semantic block that produced a chunk. It is an
// indexing hint for the later enrichment stage, not part of the persisted
// Markdown artifact contract.
type BlockKind string

const (
	BlockText  BlockKind = "text"
	BlockTable BlockKind = "table"
	BlockCode  BlockKind = "code"
	BlockImage BlockKind = "image"
)

// AssetRef and SourceLocation alias the canonical document contract instead
// of introducing a second, drift-prone splitter DTO.
type AssetRef = document.AssetRef
type SourceLocation = document.SourceLocation

// Config controls the approximate size of generated chunks.
type Config struct {
	ChunkSize                int // target token count; defaults to 512
	ChunkOverlap             int // ordinary-text overlap; defaults to 64
	EnhancementReserveTokens int // table/code space reserved for Task 11
}

func (c *Config) normalize() {
	if c.ChunkSize <= 0 {
		c.ChunkSize = 512
	}
	if c.ChunkOverlap < 0 || c.ChunkOverlap >= c.ChunkSize {
		c.ChunkOverlap = c.ChunkSize / 8
	}
	if c.EnhancementReserveTokens < 0 {
		c.EnhancementReserveTokens = 0
	}
}

func (c Config) enhancementReserve() int {
	return min(c.EnhancementReserveTokens, c.ChunkSize/5)
}

// AssetBinding retains occurrence-local data needed when Task 12 stages the
// rag_chunk_assets catalog. AssetRefs is the public projection; bindings are
// deliberately URL- and object-key-free as well.
type AssetBinding struct {
	OccurrenceID string
	Asset        AssetRef
	OCRText      string
	Order        int
}

// Chunk is a searchable piece of a canonical ParsedArtifact.
type Chunk struct {
	Index         int
	RawContent    string
	Enhancement   string
	SearchContent string
	SectionTitle  string
	Location      SourceLocation
	AssetRefs     []AssetRef
	Tokens        int

	// Kind and ReservedTokens are consumed by the later enrichment/finalize
	// stage. They do not replace or mutate RawContent.
	Kind           BlockKind
	ReservedTokens int
	AssetBindings  []AssetBinding

	// Content and PageNum keep the Phase B callers source-compatible until
	// Task 12 switches the pipeline to Split(ParsedArtifact, Config).
	Content string
	PageNum int
}

// EstimateTokens counts each CJK rune as one token and every four other runes
// as one token, rounded up. It is intentionally a deterministic estimator, not
// a claim about any provider tokenizer.
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

// splitToFit breaks oversized text at rune boundaries. A non-positive budget
// is treated as one token so tiny configurations remain total and panic-free.
func splitToFit(text string, maxTokens int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	maxTokens = max(1, maxTokens)
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

func prefixForTokens(text string, maxTokens int) string {
	if maxTokens <= 0 || text == "" {
		return ""
	}
	runes := []rune(text)
	end, cjk, other := 0, 0, 0
	for i, r := range runes {
		nextCJK, nextOther := cjk, other
		if isCJK(r) {
			nextCJK++
		} else {
			nextOther++
		}
		if nextCJK+(nextOther+3)/4 > maxTokens {
			break
		}
		cjk, other, end = nextCJK, nextOther, i+1
	}
	return string(runes[:end])
}

// suffixForTokens returns the longest rune-safe suffix within maxTokens.
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

// sectionTitleForSearch retains the complete breadcrumb whenever it fits. If
// a pathological heading competes with raw/reserved space, it keeps the most
// specific suffix in SearchContent while SectionTitle retains the full path.
func sectionTitleForSearch(title string, chunkSize int) string {
	return sectionTitleForReserve(title, chunkSize, 0)
}

func sectionTitleForReserve(title string, chunkSize, reserve int) string {
	title = strings.TrimSpace(title)
	if title == "" || chunkSize <= 0 {
		return ""
	}
	reserve = max(0, min(reserve, chunkSize-1))
	if EstimateTokens(chunktext.Search(title, ""))+reserve < chunkSize {
		return title
	}
	const ellipsis = "…"
	if EstimateTokens(chunktext.Search(ellipsis, ""))+reserve >= chunkSize {
		return ""
	}
	runes := []rune(title)
	low, high, best := 0, len(runes), 0
	for low <= high {
		middle := low + (high-low)/2
		candidate := ellipsis + string(runes[len(runes)-middle:])
		if EstimateTokens(chunktext.Search(candidate, ""))+reserve < chunkSize {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return ellipsis + string(runes[len(runes)-best:])
}

type semanticBlock struct {
	kind         BlockKind
	raw          string
	sectionTitle string
	location     SourceLocation
	bindings     []AssetBinding
	wrappers     []markdownWrapper
	table        *tableData
	code         *codeData
	boundary     bool
}

func blockPage(location SourceLocation) int {
	if location.Kind == document.LocationPage {
		return location.Index
	}
	return 0
}

func cloneBindings(bindings []AssetBinding) []AssetBinding {
	if len(bindings) == 0 {
		return nil
	}
	cloned := make([]AssetBinding, len(bindings))
	copy(cloned, bindings)
	return cloned
}

// localizeBindings assigns each occurrence to the most specific derived raw
// fragment containing its rendered caption/OCR anchor. A missing anchor falls
// back to one deterministic fragment; it is never copied across every chunk.
func localizeBindings(blocks []semanticBlock, bindings []AssetBinding) []semanticBlock {
	if len(blocks) == 0 {
		return blocks
	}
	for i := range blocks {
		blocks[i].bindings = nil
	}
	if len(bindings) == 0 {
		return blocks
	}
	cursor := 0
	for _, binding := range bindings {
		best, bestScore := -1, 0
		for i := cursor; i < len(blocks); i++ {
			score := bindingMatchScore(blocks[i].raw, binding)
			if score > bestScore || score == bestScore && score > 0 && best >= 0 &&
				len(blocks[best].bindings) > 0 && len(blocks[i].bindings) == 0 {
				best, bestScore = i, score
			}
		}
		if best < 0 {
			for i := 0; i < cursor && i < len(blocks); i++ {
				if score := bindingMatchScore(blocks[i].raw, binding); score > bestScore {
					best, bestScore = i, score
				}
			}
		}
		if best < 0 {
			for i := cursor; i < len(blocks); i++ {
				if strings.TrimSpace(blocks[i].raw) != "" && len(blocks[i].bindings) == 0 {
					best = i
					break
				}
			}
		}
		if best < 0 {
			best = min(cursor, len(blocks)-1)
		}
		blocks[best].bindings = append(blocks[best].bindings, binding)
		cursor = best
	}
	return blocks
}

func bindingMatchScore(raw string, binding AssetBinding) int {
	raw = strings.TrimSpace(raw)
	plainRaw := strings.ReplaceAll(raw, `\`, "")
	best := 0
	for _, candidate := range []struct {
		value string
		score int
	}{
		{"图片说明：" + escapePlainMarkdown(binding.Asset.Caption), 8},
		{"图片文字：" + escapePlainMarkdown(binding.OCRText), 8},
		{escapePlainMarkdown(binding.Asset.Caption), 4},
		{escapePlainMarkdown(binding.OCRText), 4},
		{strings.Join(strings.Fields(binding.Asset.Caption), " "), 2},
		{strings.Join(strings.Fields(binding.OCRText), " "), 2},
	} {
		value := strings.TrimSpace(candidate.value)
		if value == "" {
			continue
		}
		if strings.Contains(raw, value) || strings.Contains(plainRaw, strings.ReplaceAll(value, `\`, "")) {
			best = max(best, candidate.score)
		}
	}
	return best
}

func makeChunk(block semanticBlock, raw string, cfg Config, reserve int) Chunk {
	raw = strings.TrimSpace(raw)
	searchTitle := sectionTitleForReserve(block.sectionTitle, cfg.ChunkSize, reserve)
	searchContent := chunktext.Search(searchTitle, raw)
	bindings := cloneBindings(block.bindings)
	refs := make([]AssetRef, len(bindings))
	for i := range bindings {
		refs[i] = bindings[i].Asset
	}
	return Chunk{
		RawContent: raw, Content: raw, Enhancement: "", SearchContent: searchContent,
		SectionTitle: block.sectionTitle, Location: block.location,
		AssetRefs: refs, AssetBindings: bindings, Tokens: EstimateTokens(searchContent),
		Kind: block.kind, ReservedTokens: reserve, PageNum: blockPage(block.location),
	}
}

func fitsBlock(block semanticBlock, raw string, cfg Config, reserve int) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	title := sectionTitleForReserve(block.sectionTitle, cfg.ChunkSize, reserve)
	return EstimateTokens(chunktext.Search(title, strings.TrimSpace(raw)))+reserve <= cfg.ChunkSize
}

func maxRawBudget(block semanticBlock, cfg Config, reserve int) int {
	title := sectionTitleForReserve(block.sectionTitle, cfg.ChunkSize, reserve)
	prefix := EstimateTokens(chunktext.Search(title, ""))
	return max(1, cfg.ChunkSize-reserve-prefix)
}

type ordinaryPiece struct {
	text      string
	separator string
}

// splitOrdinaryCandidate always splits a candidate before it reaches the
// greedy box. This is the key invariant that prevents a nearly-full chunk plus
// one individually-large sentence from approaching 2*ChunkSize.
func splitOrdinaryCandidate(block semanticBlock, cfg Config) []ordinaryPiece {
	raw := strings.TrimSpace(block.raw)
	if raw == "" {
		return nil
	}
	budget := maxRawBudget(block, cfg, 0)
	var atoms []string
	for _, sentence := range splitSentences(raw) {
		atoms = append(atoms, splitToFit(sentence, budget)...)
	}
	if len(atoms) == 0 {
		atoms = splitToFit(raw, budget)
	}

	var pieces []ordinaryPiece
	for _, atom := range atoms {
		remaining := atom
		for remaining != "" {
			if fitsBlock(block, remaining, cfg, 0) {
				pieces = append(pieces, ordinaryPiece{text: remaining})
				break
			}
			prefix := longestPrefixThatFits(remaining, func(candidate string) bool {
				return fitsBlock(block, candidate, cfg, 0)
			})
			if prefix == "" {
				_, size := utf8.DecodeRuneInString(remaining)
				prefix = remaining[:size]
			}
			pieces = append(pieces, ordinaryPiece{text: prefix})
			remaining = remaining[len(prefix):]
		}
	}
	for i := 1; i < len(pieces); i++ {
		// Sentence pieces are exact adjacent source slices.
		pieces[i].separator = ""
	}
	return pieces
}

func longestPrefixThatFits(text string, fits func(string) bool) string {
	runes := []rune(text)
	low, high, best := 1, len(runes), 0
	for low <= high {
		middle := low + (high-low)/2
		if fits(string(runes[:middle])) {
			best = middle
			low = middle + 1
		} else {
			high = middle - 1
		}
	}
	return string(runes[:best])
}

type ordinaryBox struct {
	cfg      Config
	block    semanticBlock
	buffer   string
	hasBlock bool
	chunks   []Chunk
}

func (b *ordinaryBox) resetFor(block semanticBlock) {
	b.block = block
	b.buffer = ""
	b.hasBlock = false
}

func (b *ordinaryBox) compatible(block semanticBlock) bool {
	return b.hasBlock && b.block.sectionTitle == block.sectionTitle && b.block.location == block.location
}

func (b *ordinaryBox) emit() {
	if strings.TrimSpace(b.buffer) == "" {
		b.buffer = ""
		b.hasBlock = false
		return
	}
	b.chunks = append(b.chunks, makeChunk(b.block, b.buffer, b.cfg, 0))
	b.buffer = ""
	b.hasBlock = false
}

func (b *ordinaryBox) add(block semanticBlock) {
	if !b.compatible(block) {
		b.emit()
		b.resetFor(block)
	}
	pieces := splitOrdinaryCandidate(block, b.cfg)
	for pieceIndex, piece := range pieces {
		separator := piece.separator
		if pieceIndex == 0 && b.buffer != "" {
			separator = "\n\n"
		}
		candidate := b.buffer
		if candidate != "" {
			candidate += separator
		}
		candidate += piece.text
		if b.buffer != "" && !fitsBlock(block, candidate, b.cfg, 0) {
			previous := b.buffer
			b.emit()
			b.resetFor(block)
			b.buffer = overlapThatFits(previous, separator, piece.text, block, b.cfg)
			if b.buffer != "" {
				b.buffer += separator
			}
			b.buffer += piece.text
			b.hasBlock = true
			continue
		}
		b.buffer = candidate
		b.hasBlock = true
	}
}

func overlapThatFits(previous, separator, next string, block semanticBlock, cfg Config) string {
	maxOverlap := min(cfg.ChunkOverlap, maxRawBudget(block, cfg, 0))
	if maxOverlap <= 0 {
		return ""
	}
	overlap := suffixForTokens(previous, maxOverlap)
	for overlap != "" && !fitsBlock(block, overlap+separator+next, cfg, 0) {
		overlap = suffixForTokens(overlap, EstimateTokens(overlap)-1)
	}
	return overlap
}

func splitBlocks(blocks []semanticBlock, cfg Config) []Chunk {
	cfg.normalize()
	blocks = mergeImageNeighbors(blocks, cfg)
	box := ordinaryBox{cfg: cfg}
	for _, block := range blocks {
		if block.boundary || strings.TrimSpace(block.raw) == "" && block.table == nil && block.code == nil {
			box.emit()
			continue
		}
		switch block.kind {
		case BlockTable:
			box.emit()
			for _, splitBlock := range splitTable(block, cfg) {
				box.chunks = append(box.chunks, makeChunk(splitBlock, splitBlock.raw, cfg, cfg.enhancementReserve()))
			}
		case BlockCode:
			box.emit()
			for _, splitBlock := range splitCode(block, cfg) {
				box.chunks = append(box.chunks, makeChunk(splitBlock, splitBlock.raw, cfg, cfg.enhancementReserve()))
			}
		case BlockImage:
			box.emit()
			for _, splitBlock := range splitAtomicText(block, cfg) {
				box.chunks = append(box.chunks, makeChunk(splitBlock, splitBlock.raw, cfg, 0))
			}
		default:
			box.add(block)
		}
	}
	box.emit()
	for i := range box.chunks {
		box.chunks[i].Index = i
	}
	return box.chunks
}

func splitAtomicText(block semanticBlock, cfg Config) []semanticBlock {
	pieces := splitOrdinaryCandidate(block, cfg)
	result := make([]semanticBlock, 0, len(pieces))
	buffer := ""
	emit := func() {
		buffer = strings.TrimSpace(buffer)
		if buffer == "" {
			return
		}
		clone := block
		clone.raw = buffer
		clone.bindings = nil
		result = append(result, clone)
		buffer = ""
	}
	for _, piece := range pieces {
		candidate := buffer + piece.separator + piece.text
		if buffer != "" && !fitsBlock(block, candidate, cfg, 0) {
			emit()
			candidate = piece.text
		}
		buffer = candidate
	}
	emit()
	return localizeBindings(result, block.bindings)
}

func mergeImageNeighbors(blocks []semanticBlock, cfg Config) []semanticBlock {
	if len(blocks) == 0 {
		return nil
	}
	consumed := make([]bool, len(blocks))
	result := make([]semanticBlock, 0, len(blocks))
	shortLimit := min(64, max(1, cfg.ChunkSize/4))
	for i := range blocks {
		if consumed[i] {
			continue
		}
		if blocks[i].kind != BlockImage {
			result = append(result, blocks[i])
			continue
		}
		image := blocks[i]
		if i > 0 && !consumed[i-1] && len(result) > 0 && shortNeighbor(blocks[i-1], image, shortLimit) {
			candidate := strings.TrimSpace(blocks[i-1].raw) + "\n\n" + strings.TrimSpace(image.raw)
			if fitsBlock(image, candidate, cfg, 0) {
				image.raw = candidate
				result = result[:len(result)-1]
				consumed[i-1] = true
			}
		}
		if i+1 < len(blocks) && shortNeighbor(blocks[i+1], image, shortLimit) {
			candidate := strings.TrimSpace(image.raw) + "\n\n" + strings.TrimSpace(blocks[i+1].raw)
			if fitsBlock(image, candidate, cfg, 0) {
				image.raw = candidate
				consumed[i+1] = true
			}
		}
		result = append(result, image)
	}
	return result
}

func shortNeighbor(candidate, image semanticBlock, limit int) bool {
	return candidate.kind == BlockText && !candidate.boundary &&
		candidate.sectionTitle == image.sectionTitle && candidate.location == image.location &&
		EstimateTokens(candidate.raw) <= limit
}

// SlidingWindow greedily packs ordinary text up to ChunkSize and retains only
// ordinary-text overlap. It remains as a compatibility surface for Phase B.
func SlidingWindow(text string, cfg Config, sectionTitle string, pageNum int) []Chunk {
	cfg.normalize()
	location := document.SourceLocation{Kind: document.LocationDocument}
	if pageNum > 0 {
		location = document.SourceLocation{Kind: document.LocationPage, Index: pageNum}
	}
	return splitBlocks([]semanticBlock{{
		kind: BlockText, raw: text, sectionTitle: sectionTitle, location: location,
	}}, cfg)
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

// ResplitChunk deterministically applies the structural splitter to immutable
// raw chunk text under a smaller budget. It is used only by the post-enrichment
// provider/storage boundary check. Raw table/code syntax is reparsed so the
// resulting views remain legal Markdown; source metadata and asset bindings
// are carried forward without allowing enhancement text to replace source.
func ResplitChunk(input Chunk, cfg Config) []Chunk {
	location := input.Location
	if location.Kind == "" {
		location = document.SourceLocation{Kind: document.LocationDocument}
	}
	unit := document.MarkdownUnit{ID: "unit_resplit", Location: location, Markdown: input.RawContent}
	blocks := parseUnitBlocks(unit, artifactLookup{
		assets: map[string]document.ArtifactAsset{}, attachments: map[string]document.ArtifactAttachment{},
		occurrences: map[string]document.ArtifactOccurrence{},
	}, &headingState{})
	for i := range blocks {
		if blocks[i].boundary {
			continue
		}
		blocks[i].sectionTitle = input.SectionTitle
		blocks[i].location = location
	}
	blocks = localizeBindings(blocks, input.AssetBindings)
	if input.Kind == BlockImage {
		for i := range blocks {
			if !blocks[i].boundary && len(blocks[i].bindings) > 0 && blocks[i].kind == BlockText {
				blocks[i].kind = BlockImage
			}
		}
	}
	result := splitBlocks(blocks, cfg)
	for i := range result {
		result[i].Index = i
		result[i].Enhancement = ""
	}
	return result
}
