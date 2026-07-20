package parse

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

const (
	maxMarkdownLinkBytes = 2048
	maxImageAltBytes     = 1024
)

var safeCodeLanguage = regexp.MustCompile(`^[A-Za-z0-9_+.#-]{1,64}$`)

// NormalizeMarkdown applies the same AST-based security policy to every
// parser source. Internal asset markers are retained only when the caller has
// explicitly enabled them and the occurrence belongs to the current unit.
func NormalizeMarkdown(
	units []document.MarkdownUnit,
	occurrences map[string]document.AssetOccurrence,
	allowInternalAssets bool,
) ([]document.MarkdownUnit, []document.ParseWarning, error) {
	if len(units) == 0 {
		return []document.MarkdownUnit{}, nil, nil
	}
	result := make([]document.MarkdownUnit, len(units))
	var warnings []document.ParseWarning
	seenUnits := make(map[string]struct{}, len(units))
	for index, unit := range units {
		if strings.TrimSpace(unit.ID) == "" {
			return nil, nil, fmt.Errorf("markdown unit %d has empty id", index)
		}
		if _, exists := seenUnits[unit.ID]; exists {
			return nil, nil, fmt.Errorf("duplicate markdown unit id %q", unit.ID)
		}
		seenUnits[unit.ID] = struct{}{}
		normalized, unitWarnings, err := normalizeMarkdownUnit(unit, occurrences, allowInternalAssets)
		if err != nil {
			return nil, nil, fmt.Errorf("normalize markdown unit %q: %w", unit.ID, err)
		}
		unit.Markdown = normalized
		result[index] = unit
		warnings = append(warnings, unitWarnings...)
	}
	return result, warnings, nil
}

func normalizeMarkdownUnit(
	unit document.MarkdownUnit,
	occurrences map[string]document.AssetOccurrence,
	allowInternalAssets bool,
) (string, []document.ParseWarning, error) {
	if !utf8.ValidString(unit.Markdown) {
		return "", nil, errors.New("markdown is not valid UTF-8")
	}
	source := normalizeMarkdownSource(unit.Markdown)
	var warnings []document.ParseWarning
	for pass := 0; pass < 8; pass++ {
		normalized, passWarnings, err := renderMarkdownUnit(source, unit, occurrences, allowInternalAssets)
		if err != nil {
			return "", nil, err
		}
		if pass == 0 {
			warnings = passWarnings
		}
		if normalized == source {
			return normalized, warnings, nil
		}
		source = normalized
	}
	return "", nil, errors.New("Markdown normalization did not converge")
}

func renderMarkdownUnit(
	source string,
	unit document.MarkdownUnit,
	occurrences map[string]document.AssetOccurrence,
	allowInternalAssets bool,
) (string, []document.ParseWarning, error) {
	markdown := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(parser.WithAutoHeadingID()),
	)
	root := markdown.Parser().Parse(text.NewReader([]byte(source)))
	renderer := markdownRenderer{
		source:              []byte(source),
		unit:                unit,
		occurrences:         occurrences,
		allowInternalAssets: allowInternalAssets,
	}
	if err := renderer.renderChildren(root, &renderer.output); err != nil {
		return "", nil, err
	}
	result := strings.TrimRight(renderer.output.String(), " \t\r\n")
	if result != "" {
		result += "\n"
	}
	return result, renderer.warnings, nil
}

func normalizeMarkdownSource(value string) string {
	value = strings.TrimPrefix(value, "\ufeff")
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 {
			return r
		}
		return ' '
	}, value)
}

type markdownRenderer struct {
	source              []byte
	unit                document.MarkdownUnit
	occurrences         map[string]document.AssetOccurrence
	allowInternalAssets bool
	output              strings.Builder
	warnings            []document.ParseWarning
}

func (r *markdownRenderer) renderChildren(parent ast.Node, output *strings.Builder) error {
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		if err := r.renderBlock(child, output); err != nil {
			return err
		}
	}
	return nil
}

func (r *markdownRenderer) renderBlock(node ast.Node, output *strings.Builder) error {
	switch current := node.(type) {
	case *ast.Heading:
		output.WriteString(strings.Repeat("#", current.Level))
		output.WriteByte(' ')
		if err := r.renderInlines(current, output, false); err != nil {
			return err
		}
		writeBlockEnd(output)
	case *ast.Paragraph, *ast.TextBlock:
		if err := r.renderInlines(node, output, false); err != nil {
			return err
		}
		writeBlockEnd(output)
	case *ast.FencedCodeBlock:
		r.renderCodeBlock(current.Lines().Value(r.source), current.Language(r.source), output)
	case *ast.CodeBlock:
		r.renderCodeBlock(current.Lines().Value(r.source), nil, output)
	case *ast.Blockquote:
		var nested strings.Builder
		if err := r.renderChildren(current, &nested); err != nil {
			return err
		}
		writePrefixedBlock(output, nested.String(), "> ")
		writeBlockEnd(output)
	case *ast.List:
		if err := r.renderList(current, output); err != nil {
			return err
		}
		writeBlockEnd(output)
	case *ast.ListItem:
		return r.renderChildren(current, output)
	case *ast.ThematicBreak:
		output.WriteString("---")
		writeBlockEnd(output)
	case *ast.HTMLBlock:
		r.renderRawHTML(current.Text(r.source), output)
		writeBlockEnd(output)
	case *extast.Table:
		if err := r.renderTable(current, output); err != nil {
			return err
		}
		writeBlockEnd(output)
	default:
		if node.IsRaw() {
			r.addWarning("markdown_raw_block_removed", "raw Markdown block was escaped")
			return nil
		}
		return r.renderChildren(node, output)
	}
	return nil
}

func (r *markdownRenderer) renderInlines(parent ast.Node, output *strings.Builder, tableCell bool) error {
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		if err := r.renderInline(child, output, tableCell); err != nil {
			return err
		}
	}
	return nil
}

func (r *markdownRenderer) renderInline(node ast.Node, output *strings.Builder, tableCell bool) error {
	switch current := node.(type) {
	case *ast.Text:
		output.WriteString(escapeMarkdownText(string(current.Value(r.source)), tableCell))
		if current.HardLineBreak() {
			output.WriteString("  \n")
		} else if current.SoftLineBreak() {
			if tableCell {
				output.WriteByte(' ')
			} else {
				output.WriteByte('\n')
			}
		}
	case *ast.String:
		output.WriteString(escapeMarkdownText(string(current.Value), tableCell))
	case *ast.CodeSpan:
		value := inlinePlainText(current, r.source)
		if tableCell {
			// GFM table parsing recognizes an escaped pipe inside code spans. Keep
			// that escape when serializing or the next parse silently splits the
			// code span into another cell.
			value = strings.ReplaceAll(value, "|", `\|`)
		}
		fence := strings.Repeat("`", maxConsecutive(value, '`')+1)
		if fence == "`" && value == "" {
			fence = "``"
		}
		padding := ""
		if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") ||
			strings.HasPrefix(value, " ") || strings.HasSuffix(value, " ") {
			padding = " "
		}
		output.WriteString(fence + padding + value + padding + fence)
	case *ast.Emphasis:
		marker := "*"
		if current.Level >= 2 {
			marker = "**"
		}
		output.WriteString(marker)
		if err := r.renderInlines(current, output, tableCell); err != nil {
			return err
		}
		output.WriteString(marker)
	case *extast.Strikethrough:
		output.WriteString("~~")
		if err := r.renderInlines(current, output, tableCell); err != nil {
			return err
		}
		output.WriteString("~~")
	case *extast.TaskCheckBox:
		if current.IsChecked {
			output.WriteString("[x] ")
		} else {
			output.WriteString("[ ] ")
		}
	case *ast.Link:
		var label strings.Builder
		if err := r.renderInlines(current, &label, tableCell); err != nil {
			return err
		}
		destination, ok := safeHTTPLink(string(current.Destination))
		if !ok {
			r.addWarning("markdown_link_unsafe", "unsafe Markdown link was converted to text")
			if label.Len() > 0 {
				output.WriteString(label.String())
			} else {
				output.WriteString(escapeMarkdownText(string(current.Destination), tableCell))
			}
			return nil
		}
		output.WriteByte('[')
		output.WriteString(label.String())
		output.WriteString("](")
		output.WriteString(destination)
		output.WriteByte(')')
	case *ast.AutoLink:
		destination, ok := safeHTTPLink(string(current.URL(r.source)))
		if !ok {
			output.WriteString(escapeMarkdownText(string(current.Label(r.source)), tableCell))
			return nil
		}
		output.WriteByte('<')
		output.WriteString(destination)
		output.WriteByte('>')
	case *ast.Image:
		return r.renderImage(current, output, tableCell)
	case *ast.RawHTML:
		r.renderRawHTML(current.Segments.Value(r.source), output)
	default:
		return r.renderInlines(node, output, tableCell)
	}
	return nil
}

func (r *markdownRenderer) renderImage(image *ast.Image, output *strings.Builder, tableCell bool) error {
	alt := normalizeAltText(inlinePlainText(image, r.source))
	destination := strings.TrimSpace(string(image.Destination))
	if strings.HasPrefix(strings.ToLower(destination), "rag-visual://") {
		return errors.New("unresolved internal visual marker")
	}
	if strings.HasPrefix(strings.ToLower(destination), "rag-asset://") {
		occurrenceID := strings.TrimPrefix(destination, "rag-asset://")
		occurrence, exists := r.occurrences[occurrenceID]
		if r.allowInternalAssets {
			if !exists || occurrence.ID != occurrenceID || occurrence.UnitID != r.unit.ID {
				return errors.New("unknown or cross-unit asset occurrence")
			}
			output.WriteString("![")
			output.WriteString(escapeImageAlt(alt, tableCell))
			output.WriteString("](rag-asset://")
			output.WriteString(occurrenceID)
			output.WriteByte(')')
			return nil
		}
	}
	r.addWarning("markdown_image_ignored", "document image was ignored without fetching")
	output.WriteString(ignoredImagePlaceholder(alt, tableCell))
	return nil
}

func (r *markdownRenderer) renderCodeBlock(content, language []byte, output *strings.Builder) {
	value := strings.ReplaceAll(string(content), "\r\n", "\n")
	value = strings.ReplaceAll(value, "\r", "\n")
	fence := strings.Repeat("`", max(3, maxConsecutive(value, '`')+1))
	output.WriteString(fence)
	if safeCodeLanguage.Match(language) {
		output.Write(language)
	}
	output.WriteByte('\n')
	output.WriteString(value)
	if value != "" && !strings.HasSuffix(value, "\n") {
		output.WriteByte('\n')
	}
	output.WriteString(fence)
	writeBlockEnd(output)
}

func (r *markdownRenderer) renderList(list *ast.List, output *strings.Builder) error {
	itemNumber := list.Start
	if itemNumber <= 0 {
		itemNumber = 1
	}
	for child := list.FirstChild(); child != nil; child = child.NextSibling() {
		item, ok := child.(*ast.ListItem)
		if !ok {
			continue
		}
		var nested strings.Builder
		if err := r.renderChildren(item, &nested); err != nil {
			return err
		}
		marker := "- "
		if list.IsOrdered() {
			marker = strconv.Itoa(itemNumber) + ". "
			itemNumber++
		}
		value := strings.TrimRight(nested.String(), " \t\r\n")
		lines := strings.Split(value, "\n")
		if len(lines) == 0 {
			lines = []string{""}
		}
		output.WriteString(marker)
		output.WriteString(lines[0])
		output.WriteByte('\n')
		indent := strings.Repeat(" ", len(marker))
		for _, line := range lines[1:] {
			if line == "" {
				output.WriteByte('\n')
				continue
			}
			output.WriteString(indent)
			output.WriteString(line)
			output.WriteByte('\n')
		}
	}
	return nil
}

func (r *markdownRenderer) renderTable(table *extast.Table, output *strings.Builder) error {
	var rows [][]string
	var header []string
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		switch current := child.(type) {
		case *extast.TableHeader:
			cells, err := r.renderTableCells(current)
			if err != nil {
				return err
			}
			header = cells
		case *extast.TableRow:
			cells, err := r.renderTableCells(current)
			if err != nil {
				return err
			}
			rows = append(rows, cells)
		}
	}
	if len(header) == 0 {
		return errors.New("GFM table has no header")
	}
	writeTableRow(output, header)
	output.WriteByte('|')
	for index := range header {
		alignment := extast.AlignNone
		if index < len(table.Alignments) {
			alignment = table.Alignments[index]
		}
		switch alignment {
		case extast.AlignLeft:
			output.WriteString(" :--- |")
		case extast.AlignRight:
			output.WriteString(" ---: |")
		case extast.AlignCenter:
			output.WriteString(" :---: |")
		default:
			output.WriteString(" --- |")
		}
	}
	output.WriteByte('\n')
	for _, row := range rows {
		for len(row) < len(header) {
			row = append(row, "")
		}
		writeTableRow(output, row[:len(header)])
	}
	return nil
}

func (r *markdownRenderer) renderTableCells(parent ast.Node) ([]string, error) {
	var cells []string
	for child := parent.FirstChild(); child != nil; child = child.NextSibling() {
		cell, ok := child.(*extast.TableCell)
		if !ok {
			continue
		}
		var value strings.Builder
		if err := r.renderInlines(cell, &value, true); err != nil {
			return nil, err
		}
		cells = append(cells, strings.TrimSpace(value.String()))
	}
	return cells, nil
}

func (r *markdownRenderer) renderRawHTML(raw []byte, output *strings.Builder) {
	value := string(raw)
	if looksLikeHTMLResource(value) {
		r.addWarning("markdown_image_ignored", "HTML image/resource was ignored without fetching")
		output.WriteString(ignoredImagePlaceholder("", false))
		return
	}
	r.addWarning("markdown_raw_html_removed", "raw HTML was removed")
	output.WriteString(`\[已移除不安全 HTML\]`)
}

func (r *markdownRenderer) addWarning(code, message string) {
	location := r.unit.Location
	r.warnings = append(r.warnings, document.ParseWarning{
		Code: code, Message: message, Location: &location, Degraded: true,
	})
}

func safeHTTPLink(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxMarkdownLinkBytes || strings.HasPrefix(raw, "//") {
		return "", false
	}
	for _, char := range raw {
		if char < 0x20 || char == 0x7f {
			return "", false
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", false
	}
	encoded := parsed.String()
	encoded = strings.NewReplacer("(", "%28", ")", "%29", "\\", "%5C").Replace(encoded)
	if len(encoded) > maxMarkdownLinkBytes {
		return "", false
	}
	return encoded, true
}

func ignoredImagePlaceholder(alt string, tableCell bool) string {
	if alt == "" {
		return `\[已忽略文档中的图片\]`
	}
	alt = strings.NewReplacer("[", "［", "]", "］").Replace(alt)
	alt = escapeMarkdownText(alt, tableCell)
	return `\[已忽略文档中的图片：` + alt + `\]`
}

func normalizeAltText(value string) string {
	// Goldmark Text.Value preserves source backslash escapes inside image alt
	// text. Reduce them to their logical punctuation before emitting one
	// canonical escape layer, otherwise repeated normalization grows slashes.
	value = string(util.UnescapePunctuations([]byte(value)))
	value = strings.Join(strings.Fields(value), " ")
	value = strings.TrimSpace(value)
	for len(value) > maxImageAltBytes {
		_, size := utf8.DecodeLastRuneInString(value)
		value = value[:len(value)-size]
	}
	return value
}

func escapeImageAlt(value string, tableCell bool) string {
	value = strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(value)
	if tableCell {
		value = strings.ReplaceAll(value, "|", `\|`)
	}
	return value
}

func escapeMarkdownText(value string, tableCell bool) string {
	var output strings.Builder
	for index := 0; index < len(value); {
		if entity := trustedTextEntity(value[index:]); entity != "" {
			output.WriteString(entity)
			index += len(entity)
			continue
		}
		char, size := utf8.DecodeRuneInString(value[index:])
		next := index + size
		if char == '\\' && next < len(value) {
			nextChar, nextSize := utf8.DecodeRuneInString(value[next:])
			if isMarkdownEscapeTarget(nextChar) {
				output.WriteRune(char)
				output.WriteRune(nextChar)
				index = next + nextSize
				continue
			}
		}
		switch char {
		case '&':
			output.WriteString("&amp;")
		case '<':
			output.WriteString("&lt;")
		case '>':
			output.WriteString("&gt;")
		case '\\', '*', '_', '`', '~', '[', ']', '#', '+', '-', '.':
			output.WriteByte('\\')
			output.WriteRune(char)
		case '!':
			if next < len(value) && value[next] == '[' {
				output.WriteByte('\\')
			}
			output.WriteRune(char)
		case '|':
			if tableCell {
				output.WriteByte('\\')
			}
			output.WriteRune(char)
		default:
			output.WriteRune(char)
		}
		index = next
	}
	return output.String()
}

func trustedTextEntity(value string) string {
	for _, entity := range []string{"&amp;", "&lt;", "&gt;"} {
		if strings.HasPrefix(value, entity) {
			return entity
		}
	}
	return ""
}

func isMarkdownEscapeTarget(char rune) bool {
	return strings.ContainsRune(`\\`+"`*{}[]<>()#+-.!_|~", char)
}

func inlinePlainText(parent ast.Node, source []byte) string {
	var output strings.Builder
	_ = ast.Walk(parent, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || node == parent {
			return ast.WalkContinue, nil
		}
		switch current := node.(type) {
		case *ast.Text:
			output.Write(current.Value(source))
			if current.SoftLineBreak() || current.HardLineBreak() {
				output.WriteByte(' ')
			}
		case *ast.String:
			output.Write(current.Value)
		}
		return ast.WalkContinue, nil
	})
	return output.String()
}

func looksLikeHTMLResource(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range []string{"<img", "<picture", "<source", "<svg", "<image", "url("} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func writeBlockEnd(output *strings.Builder) {
	value := output.String()
	if strings.HasSuffix(value, "\n\n") {
		return
	}
	if strings.HasSuffix(value, "\n") {
		output.WriteByte('\n')
	} else {
		output.WriteString("\n\n")
	}
}

func writePrefixedBlock(output *strings.Builder, value, prefix string) {
	value = strings.TrimRight(value, "\r\n")
	for _, line := range strings.Split(value, "\n") {
		if line == "" {
			output.WriteString(strings.TrimSpace(prefix))
		} else {
			output.WriteString(prefix)
			output.WriteString(line)
		}
		output.WriteByte('\n')
	}
}

func writeTableRow(output *strings.Builder, cells []string) {
	output.WriteByte('|')
	for _, cell := range cells {
		output.WriteByte(' ')
		output.WriteString(cell)
		output.WriteString(" |")
	}
	output.WriteByte('\n')
}

func maxConsecutive(value string, target byte) int {
	best, current := 0, 0
	for index := 0; index < len(value); index++ {
		if value[index] == target {
			current++
			if current > best {
				best = current
			}
		} else {
			current = 0
		}
	}
	return best
}
