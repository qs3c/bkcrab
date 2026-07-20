package split

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

var safeInfoString = regexp.MustCompile(`^[A-Za-z0-9_+.-]{1,64}$`)

type headingState struct {
	titles [6]string
}

func (h *headingState) update(level int, title string) string {
	if level < 1 || level > len(h.titles) {
		return h.breadcrumb()
	}
	h.titles[level-1] = strings.TrimSpace(title)
	for i := level; i < len(h.titles); i++ {
		h.titles[i] = ""
	}
	return h.breadcrumb()
}

func (h *headingState) breadcrumb() string {
	parts := make([]string, 0, len(h.titles))
	for _, title := range h.titles {
		if title != "" {
			parts = append(parts, title)
		}
	}
	return strings.Join(parts, " > ")
}

type artifactLookup struct {
	assets      map[string]document.ArtifactAsset
	occurrences map[string]document.ArtifactOccurrence
}

func newArtifactLookup(artifact *document.ParsedArtifact) artifactLookup {
	lookup := artifactLookup{
		assets:      make(map[string]document.ArtifactAsset),
		occurrences: make(map[string]document.ArtifactOccurrence),
	}
	if artifact == nil {
		return lookup
	}
	for _, asset := range artifact.Assets {
		lookup.assets[asset.ID] = asset
	}
	for _, occurrence := range artifact.Occurrences {
		lookup.occurrences[occurrence.ID] = occurrence
	}
	return lookup
}

// Split consumes only the canonical ParsedArtifact contract. Source format is
// intentionally absent: every unit goes through the same Goldmark+GFM AST.
func Split(artifact document.ParsedArtifact, cfg Config) []Chunk {
	return SplitArtifact(&artifact, cfg)
}

// SplitArtifact is the pointer-friendly form used by pipeline orchestration.
func SplitArtifact(artifact *document.ParsedArtifact, cfg Config) []Chunk {
	if artifact == nil {
		return nil
	}
	lookup := newArtifactLookup(artifact)
	headings := &headingState{}
	var blocks []semanticBlock
	for _, unit := range artifact.Units {
		blocks = append(blocks, parseUnitBlocks(unit, lookup, headings)...)
		// A source unit is the smallest location-bearing catalog value. Never
		// let greedy packing or overlap smear one page/slide/sheet into another.
		blocks = append(blocks, semanticBlock{boundary: true})
	}
	return splitBlocks(blocks, cfg)
}

// Markdown remains the compatibility entry point for callers that already
// hold one normalized Markdown string.
func Markdown(markdown string, cfg Config) []Chunk {
	unit := document.MarkdownUnit{
		ID:       "unit_document",
		Location: document.SourceLocation{Kind: document.LocationDocument},
		Markdown: normalizeNewlines(markdown),
	}
	return splitBlocks(parseUnitBlocks(unit, artifactLookup{
		assets: map[string]document.ArtifactAsset{}, occurrences: map[string]document.ArtifactOccurrence{},
	}, &headingState{}), cfg)
}

func normalizeNewlines(value string) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	return strings.ReplaceAll(value, "\r", "\n")
}

func parseUnitBlocks(unit document.MarkdownUnit, lookup artifactLookup, headings *headingState) []semanticBlock {
	source := []byte(normalizeNewlines(unit.Markdown))
	markdown := goldmark.New(goldmark.WithExtensions(extension.GFM))
	root := markdown.Parser().Parse(text.NewReader(source))
	var blocks []semanticBlock
	for node := root.FirstChild(); node != nil; node = node.NextSibling() {
		if heading, ok := node.(*ast.Heading); ok {
			title := strings.Join(strings.Fields(string(heading.Text(source))), " ")
			headings.update(heading.Level, title)
			// A heading is a structural boundary. Its complete path is retained
			// on following chunks and in provisional SearchContent.
			blocks = append(blocks, semanticBlock{boundary: true})
			continue
		}
		if _, ok := node.(*ast.ThematicBreak); ok {
			blocks = append(blocks, semanticBlock{boundary: true})
			continue
		}
		blocks = append(blocks, parseSemanticNode(
			node, source, unit.ID, lookup, headings.breadcrumb(), unit.Location, nil,
		)...)
	}
	return removeDuplicateImageOCRBlocks(blocks)
}

type markdownWrapperKind uint8

const (
	wrapperBlockquote markdownWrapperKind = iota + 1
	wrapperListItem
)

type markdownWrapper struct {
	kind   markdownWrapperKind
	marker string
}

func appendMarkdownWrapper(wrappers []markdownWrapper, wrapper markdownWrapper) []markdownWrapper {
	result := make([]markdownWrapper, len(wrappers), len(wrappers)+1)
	copy(result, wrappers)
	return append(result, wrapper)
}

func wrapSemanticRaw(block semanticBlock, raw string) string {
	for i := len(block.wrappers) - 1; i >= 0; i-- {
		raw = block.wrappers[i].apply(raw)
	}
	return raw
}

func (w markdownWrapper) apply(raw string) string {
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return raw
	}
	switch w.kind {
	case wrapperBlockquote:
		for i := range lines {
			if lines[i] == "" {
				lines[i] = ">"
			} else {
				lines[i] = "> " + lines[i]
			}
		}
		return strings.Join(lines, "\n")
	case wrapperListItem:
		marker := w.marker
		if marker == "" {
			marker = "- "
		}
		indent := strings.Repeat(" ", len(marker))
		lines[0] = marker + lines[0]
		for i := 1; i < len(lines); i++ {
			if lines[i] != "" {
				lines[i] = indent + lines[i]
			}
		}
		return strings.Join(lines, "\n")
	default:
		return raw
	}
}

// parseSemanticNode recursively preserves code/table nodes nested in
// blockquotes and list items. Keeping their structure separate lets the
// specialized splitters close every fence and repeat every table header.
func parseSemanticNode(
	node ast.Node,
	source []byte,
	unitID string,
	lookup artifactLookup,
	sectionTitle string,
	location SourceLocation,
	wrappers []markdownWrapper,
) []semanticBlock {
	switch current := node.(type) {
	case *ast.Blockquote:
		nested := appendMarkdownWrapper(wrappers, markdownWrapper{kind: wrapperBlockquote})
		var blocks []semanticBlock
		for child := current.FirstChild(); child != nil; child = child.NextSibling() {
			blocks = append(blocks, parseSemanticNode(child, source, unitID, lookup, sectionTitle, location, nested)...)
		}
		return blocks
	case *ast.List:
		number := current.Start
		if number <= 0 {
			number = 1
		}
		var blocks []semanticBlock
		for item := current.FirstChild(); item != nil; item = item.NextSibling() {
			marker := "- "
			if current.IsOrdered() {
				marker = fmt.Sprintf("%d%c ", number, current.Marker)
				number++
			}
			nested := appendMarkdownWrapper(wrappers, markdownWrapper{kind: wrapperListItem, marker: marker})
			for child := item.FirstChild(); child != nil; child = child.NextSibling() {
				blocks = append(blocks, parseSemanticNode(child, source, unitID, lookup, sectionTitle, location, nested)...)
			}
		}
		return blocks
	case *ast.ThematicBreak:
		return []semanticBlock{{boundary: true}}
	}

	block := semanticBlock{
		kind: BlockText, sectionTitle: sectionTitle, location: location,
		wrappers: append([]markdownWrapper(nil), wrappers...),
	}
	switch current := node.(type) {
	case *extast.Table:
		block.kind = BlockTable
		block.table = parseTableData(current, source, unitID, lookup)
		inner, bindings := block.table.renderRows(block.table.rows)
		block.raw, block.bindings = wrapSemanticRaw(block, inner), bindings
	case *ast.FencedCodeBlock:
		block.kind = BlockCode
		block.code = &codeData{
			content:  string(current.Lines().Value(source)),
			language: safeLanguage(string(current.Language(source))),
		}
		block.raw = wrapSemanticRaw(block, serializeCode(block.code.content, block.code.language))
	case *ast.CodeBlock:
		block.kind = BlockCode
		block.code = &codeData{content: string(current.Lines().Value(source))}
		block.raw = wrapSemanticRaw(block, serializeCode(block.code.content, ""))
	default:
		rendered := renderBlockNode(node, source, unitID, lookup)
		block.raw = wrapSemanticRaw(block, strings.TrimSpace(rendered.text))
		block.bindings = rendered.bindings
		if len(block.bindings) > 0 {
			block.kind = BlockImage
		}
	}
	if strings.TrimSpace(block.raw) == "" {
		return nil
	}
	return []semanticBlock{block}
}

func safeLanguage(language string) string {
	language = strings.TrimSpace(language)
	if safeInfoString.MatchString(language) {
		return language
	}
	return ""
}

type renderedMarkdown struct {
	text     string
	bindings []AssetBinding
}

func renderBlockNode(node ast.Node, source []byte, unitID string, lookup artifactLookup) renderedMarkdown {
	switch current := node.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		return renderInlineChildren(node, source, unitID, lookup, false)
	case *ast.List:
		return renderList(current, source, unitID, lookup)
	case *ast.Blockquote:
		inner := renderBlockChildren(current, source, unitID, lookup)
		lines := strings.Split(strings.TrimSpace(inner.text), "\n")
		for i := range lines {
			if lines[i] == "" {
				lines[i] = ">"
			} else {
				lines[i] = "> " + lines[i]
			}
		}
		inner.text = strings.Join(lines, "\n")
		return inner
	case *ast.FencedCodeBlock:
		return renderedMarkdown{text: serializeCode(string(current.Lines().Value(source)), safeLanguage(string(current.Language(source))))}
	case *ast.CodeBlock:
		return renderedMarkdown{text: serializeCode(string(current.Lines().Value(source)), "")}
	case *extast.Table:
		table := parseTableData(current, source, unitID, lookup)
		raw, bindings := table.renderRows(table.rows)
		return renderedMarkdown{text: raw, bindings: bindings}
	case *ast.HTMLBlock:
		// Task 7 normally turns raw HTML into ordinary escaped text. Keep a
		// defensive visible representation if an older artifact reaches us.
		return renderedMarkdown{text: escapePlainMarkdown(string(current.Text(source)))}
	default:
		if node.FirstChild() != nil {
			return renderBlockChildren(node, source, unitID, lookup)
		}
		if node.Type() == ast.TypeBlock && node.Lines().Len() > 0 {
			return renderedMarkdown{text: strings.TrimSpace(string(node.Lines().Value(source)))}
		}
		return renderedMarkdown{}
	}
}

func renderBlockChildren(parent ast.Node, source []byte, unitID string, lookup artifactLookup) renderedMarkdown {
	var builder strings.Builder
	var bindings []AssetBinding
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		rendered := renderBlockNode(child, source, unitID, lookup)
		if strings.TrimSpace(rendered.text) == "" {
			continue
		}
		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(rendered.text)
		bindings = append(bindings, rendered.bindings...)
	}
	return renderedMarkdown{text: builder.String(), bindings: bindings}
}

func renderList(list *ast.List, source []byte, unitID string, lookup artifactLookup) renderedMarkdown {
	var output strings.Builder
	var bindings []AssetBinding
	number := list.Start
	if number <= 0 {
		number = 1
	}
	for item := list.FirstChild(); item != nil; item = item.NextSibling() {
		rendered := renderBlockChildren(item, source, unitID, lookup)
		if strings.TrimSpace(rendered.text) == "" {
			continue
		}
		marker := "- "
		if list.IsOrdered() {
			marker = fmt.Sprintf("%d%c ", number, list.Marker)
			number++
		}
		if output.Len() > 0 {
			output.WriteByte('\n')
		}
		lines := strings.Split(rendered.text, "\n")
		output.WriteString(marker)
		output.WriteString(lines[0])
		indent := strings.Repeat(" ", len(marker))
		for _, line := range lines[1:] {
			output.WriteByte('\n')
			if line != "" {
				output.WriteString(indent)
				output.WriteString(line)
			}
		}
		bindings = append(bindings, rendered.bindings...)
	}
	return renderedMarkdown{text: output.String(), bindings: bindings}
}

func renderInlineChildren(parent ast.Node, source []byte, unitID string, lookup artifactLookup, tableCell bool) renderedMarkdown {
	var output strings.Builder
	var bindings []AssetBinding
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		rendered := renderInlineNode(child, source, unitID, lookup, tableCell)
		output.WriteString(rendered.text)
		bindings = append(bindings, rendered.bindings...)
	}
	return renderedMarkdown{text: output.String(), bindings: bindings}
}

func renderInlineNode(node ast.Node, source []byte, unitID string, lookup artifactLookup, tableCell bool) renderedMarkdown {
	switch current := node.(type) {
	case *ast.Text:
		value := string(current.Segment.Value(source))
		if tableCell {
			value = strings.ReplaceAll(value, "|", `\|`)
		}
		if current.HardLineBreak() {
			value += "  \n"
		} else if current.SoftLineBreak() {
			value += "\n"
		}
		return renderedMarkdown{text: value}
	case *ast.String:
		value := string(current.Value)
		if tableCell {
			value = strings.ReplaceAll(value, "|", `\|`)
		}
		return renderedMarkdown{text: value}
	case *ast.CodeSpan:
		value := string(current.Text(source))
		if tableCell {
			value = strings.ReplaceAll(value, "|", `\|`)
		}
		fence := strings.Repeat("`", max(1, longestRun(value, '`')+1))
		if value == "" {
			fence = "``"
		}
		padding := ""
		if strings.HasPrefix(value, " ") || strings.HasSuffix(value, " ") ||
			strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") {
			padding = " "
		}
		return renderedMarkdown{text: fence + padding + value + padding + fence}
	case *ast.Emphasis:
		inner := renderInlineChildren(current, source, unitID, lookup, tableCell)
		marker := "*"
		if current.Level >= 2 {
			marker = "**"
		}
		inner.text = marker + inner.text + marker
		return inner
	case *extast.Strikethrough:
		inner := renderInlineChildren(current, source, unitID, lookup, tableCell)
		inner.text = "~~" + inner.text + "~~"
		return inner
	case *extast.TaskCheckBox:
		if current.IsChecked {
			return renderedMarkdown{text: "[x] "}
		}
		return renderedMarkdown{text: "[ ] "}
	case *ast.Link:
		inner := renderInlineChildren(current, source, unitID, lookup, tableCell)
		inner.text = "[" + inner.text + "](" + string(current.Destination) + ")"
		return inner
	case *ast.AutoLink:
		return renderedMarkdown{text: "<" + string(current.URL(source)) + ">"}
	case *ast.Image:
		return renderInternalImage(current, source, unitID, lookup, tableCell)
	case *ast.RawHTML:
		return renderedMarkdown{text: escapePlainMarkdown(string(current.Segments.Value(source)))}
	default:
		return renderInlineChildren(node, source, unitID, lookup, tableCell)
	}
}

func renderInternalImage(image *ast.Image, source []byte, unitID string, lookup artifactLookup, tableCell bool) renderedMarkdown {
	destination := strings.TrimSpace(string(image.Destination))
	if !strings.HasPrefix(destination, "rag-asset://") {
		return renderedMarkdown{text: escapePlainMarkdown(strings.TrimSpace(string(image.Text(source))))}
	}
	occurrenceID := strings.TrimPrefix(destination, "rag-asset://")
	occurrence, ok := lookup.occurrences[occurrenceID]
	if !ok || occurrence.ID != occurrenceID || occurrence.UnitID != unitID || occurrence.Decorative {
		return renderedMarkdown{}
	}
	asset, ok := lookup.assets[occurrence.AssetID]
	if !ok {
		return renderedMarkdown{}
	}
	page := 0
	if occurrence.Location.Kind == document.LocationPage {
		page = occurrence.Location.Index
	}
	ref := document.AssetRef{
		ID: occurrence.AssetID, Kind: asset.Kind, Caption: occurrence.Caption,
		PageNum: page, Location: occurrence.Location, Width: asset.Width,
		Height: asset.Height, MIMEType: asset.SourceMIME,
	}
	binding := AssetBinding{
		OccurrenceID: occurrence.ID, Asset: ref, OCRText: occurrence.OCRText, Order: occurrence.Order,
	}
	var bindings []AssetBinding
	if asset.DisplayStatus == document.DisplayReady {
		bindings = []AssetBinding{binding}
	}
	caption := escapePlainMarkdown(occurrence.Caption)
	ocr := escapePlainMarkdown(occurrence.OCRText)
	if tableCell {
		caption = strings.ReplaceAll(caption, "|", `\|`)
		ocr = strings.ReplaceAll(ocr, "|", `\|`)
		parts := make([]string, 0, 2)
		if caption != "" {
			parts = append(parts, "图片说明："+caption)
		}
		if ocr != "" {
			parts = append(parts, "图片文字："+ocr)
		}
		return renderedMarkdown{text: strings.Join(parts, "；"), bindings: bindings}
	}
	parts := make([]string, 0, 2)
	if caption != "" {
		parts = append(parts, "图片说明："+caption)
	}
	if ocr != "" {
		parts = append(parts, "> 图片文字："+ocr)
	}
	return renderedMarkdown{text: strings.Join(parts, "\n\n"), bindings: bindings}
}

func escapePlainMarkdown(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	replacer := strings.NewReplacer(
		`\`, `\\`, `*`, `\*`, `_`, `\_`, `[`, `\[`, `]`, `\]`,
		"<", `\<`, ">", `\>`, "#", `\#`, "|", `\|`,
	)
	return replacer.Replace(value)
}

func longestRun(value string, target rune) int {
	best, current := 0, 0
	for _, r := range value {
		if r == target {
			current++
			best = max(best, current)
		} else {
			current = 0
		}
	}
	return best
}

func removeDuplicateImageOCRBlocks(blocks []semanticBlock) []semanticBlock {
	if len(blocks) < 2 {
		return blocks
	}
	result := make([]semanticBlock, 0, len(blocks))
	for i := 0; i < len(blocks); i++ {
		current := blocks[i]
		result = append(result, current)
		if current.kind != BlockImage || i+1 >= len(blocks) || blocks[i+1].kind != BlockText {
			continue
		}
		var expected []string
		for _, binding := range current.bindings {
			if value := strings.TrimSpace(binding.OCRText); value != "" {
				expected = append(expected, "图片文字："+value)
			}
		}
		visible := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(blocks[i+1].raw), ">"))
		visible = strings.ReplaceAll(visible, `\`, "")
		if len(expected) == 1 && visible == expected[0] {
			i++
		}
	}
	return result
}
