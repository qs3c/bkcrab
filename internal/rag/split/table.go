package split

import (
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
)

type tableCell struct {
	text     string
	bindings []AssetBinding
}

type tableRow struct {
	cells    []tableCell
	bindings []AssetBinding
}

type tableData struct {
	header     tableRow
	rows       []tableRow
	alignments []extast.Alignment
}

func parseTableData(table *extast.Table, source []byte, unitID string, lookup artifactLookup) *tableData {
	result := &tableData{alignments: append([]extast.Alignment(nil), table.Alignments...)}
	for child := table.FirstChild(); child != nil; child = child.NextSibling() {
		switch row := child.(type) {
		case *extast.TableHeader:
			result.header = parseTableRow(row, source, unitID, lookup)
		case *extast.TableRow:
			result.rows = append(result.rows, parseTableRow(row, source, unitID, lookup))
		}
	}
	columns := len(result.header.cells)
	if columns == 0 {
		columns = len(result.alignments)
	}
	if columns == 0 {
		columns = 1
	}
	result.header.cells = padTableCells(result.header.cells, columns)
	for i := range result.rows {
		result.rows[i].cells = padTableCells(result.rows[i].cells, columns)
	}
	for len(result.alignments) < columns {
		result.alignments = append(result.alignments, extast.AlignNone)
	}
	return result
}

func parseTableRow(row ast.Node, source []byte, unitID string, lookup artifactLookup) tableRow {
	var result tableRow
	for child := row.FirstChild(); child != nil; child = child.NextSibling() {
		cell, ok := child.(*extast.TableCell)
		if !ok {
			continue
		}
		rendered := renderInlineChildren(cell, source, unitID, lookup, true)
		text := strings.Join(strings.Fields(rendered.text), " ")
		result.cells = append(result.cells, tableCell{text: text, bindings: rendered.bindings})
		result.bindings = append(result.bindings, rendered.bindings...)
	}
	return result
}

func padTableCells(cells []tableCell, columns int) []tableCell {
	if len(cells) >= columns {
		return cells[:columns]
	}
	result := append([]tableCell(nil), cells...)
	for len(result) < columns {
		result = append(result, tableCell{})
	}
	return result
}

func (table *tableData) renderRows(rows []tableRow) (string, []AssetBinding) {
	if table == nil {
		return "", nil
	}
	var output strings.Builder
	writeTableRow(&output, table.header.cells)
	output.WriteByte('\n')
	writeTableDelimiter(&output, table.alignments, len(table.header.cells))
	bindings := cloneBindings(table.header.bindings)
	for _, row := range rows {
		output.WriteByte('\n')
		writeTableRow(&output, row.cells)
		bindings = append(bindings, row.bindings...)
	}
	return output.String(), bindings
}

func writeTableRow(output *strings.Builder, cells []tableCell) {
	output.WriteByte('|')
	for _, cell := range cells {
		output.WriteByte(' ')
		output.WriteString(escapeTableCell(cell.text))
		output.WriteString(" |")
	}
}

func writeTableDelimiter(output *strings.Builder, alignments []extast.Alignment, columns int) {
	output.WriteByte('|')
	for i := 0; i < columns; i++ {
		alignment := extast.AlignNone
		if i < len(alignments) {
			alignment = alignments[i]
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
}

func escapeTableCell(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	// The inline renderer already escapes parsed pipes. Only escape genuinely
	// unescaped pipes here, avoiding an ever-growing backslash prefix.
	var output strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] == '|' && (index == 0 || value[index-1] != '\\') {
			output.WriteByte('\\')
		}
		output.WriteByte(value[index])
	}
	return strings.TrimSpace(output.String())
}

func splitTable(block semanticBlock, cfg Config) []semanticBlock {
	if block.table == nil {
		return splitAtomicText(block, cfg)
	}
	reserve := cfg.enhancementReserve()
	headerOnly, _ := block.table.renderRows(nil)
	if !fitsBlock(block, wrapSemanticRaw(block, headerOnly), cfg, reserve) {
		return splitOversizedTableView(block, cfg)
	}

	var result []semanticBlock
	var packed []tableRow
	emit := func() {
		if len(packed) == 0 {
			return
		}
		inner, bindings := block.table.renderRows(packed)
		clone := block
		clone.raw = wrapSemanticRaw(block, inner)
		clone.bindings = bindings
		result = append(result, clone)
		packed = nil
	}

	for _, row := range block.table.rows {
		candidateRows := append(append([]tableRow(nil), packed...), row)
		candidateInner, _ := block.table.renderRows(candidateRows)
		candidate := wrapSemanticRaw(block, candidateInner)
		if fitsBlock(block, candidate, cfg, reserve) {
			packed = candidateRows
			continue
		}
		emit()
		singleInner, _ := block.table.renderRows([]tableRow{row})
		single := wrapSemanticRaw(block, singleInner)
		if fitsBlock(block, single, cfg, reserve) {
			packed = []tableRow{row}
			continue
		}
		fragments, ok := splitOversizedTableRow(block, row, cfg)
		if !ok {
			return splitOversizedTableView(block, cfg)
		}
		for _, fragment := range fragments {
			inner, bindings := block.table.renderRows([]tableRow{fragment})
			clone := block
			clone.raw = wrapSemanticRaw(block, inner)
			clone.bindings = bindings
			result = append(result, clone)
		}
	}
	emit()
	if len(result) == 0 {
		clone := block
		inner, bindings := block.table.renderRows(nil)
		clone.raw, clone.bindings = wrapSemanticRaw(block, inner), bindings
		result = append(result, clone)
	}
	return result
}

func splitOversizedTableRow(block semanticBlock, row tableRow, cfg Config) ([]tableRow, bool) {
	remaining := make([]string, len(row.cells))
	for i, cell := range row.cells {
		remaining[i] = cell.text
	}
	reserve := cfg.enhancementReserve()
	var result []tableRow
	for hasRemainingTableText(remaining) {
		maxAllowance := maxRawBudget(block, cfg, reserve)
		low, high := 1, max(1, maxAllowance)
		var best tableRow
		for low <= high {
			allowance := low + (high-low)/2
			candidate := tableRow{cells: make([]tableCell, len(row.cells)), bindings: cloneBindings(row.bindings)}
			for i := range remaining {
				candidate.cells[i] = tableCell{text: prefixForTokens(remaining[i], allowance)}
			}
			inner, _ := block.table.renderRows([]tableRow{candidate})
			if fitsBlock(block, wrapSemanticRaw(block, inner), cfg, reserve) {
				best = candidate
				low = allowance + 1
			} else {
				high = allowance - 1
			}
		}
		if !tableRowHasText(best) {
			// Header+one-rune cannot fit. Ask the caller to use the legal
			// one-column view for the complete table instead of emitting an
			// over-budget or structurally corrupt original-width row.
			return nil, false
		}
		for i := range best.cells {
			consumed := best.cells[i].text
			remaining[i] = remaining[i][len(consumed):]
		}
		result = append(result, best)
	}
	fragments := make([]semanticBlock, len(result))
	for i := range result {
		inner, _ := block.table.renderRows([]tableRow{result[i]})
		fragments[i] = block
		fragments[i].raw = wrapSemanticRaw(block, inner)
		fragments[i].bindings = nil
	}
	fragments = localizeBindings(fragments, row.bindings)
	for i := range result {
		result[i].bindings = cloneBindings(fragments[i].bindings)
		for cellIndex := range result[i].cells {
			result[i].cells[cellIndex].bindings = nil
		}
	}
	return result, true
}

func hasRemainingTableText(cells []string) bool {
	for _, cell := range cells {
		if cell != "" {
			return true
		}
	}
	return false
}

func tableRowHasText(row tableRow) bool {
	for _, cell := range row.cells {
		if cell.text != "" {
			return true
		}
	}
	return false
}

// splitOversizedTableView handles a header or column layout that cannot fit as
// a legal repeated-header GFM table. It degrades explicitly to closed fenced
// `table-gfm-fragment` views, splitting at source line/rune boundaries. Unlike
// the former synthetic |x| table, these fragments never pretend that arbitrary
// serialized bytes are a real row with a fabricated header.
func splitOversizedTableView(block semanticBlock, cfg Config) []semanticBlock {
	if block.table == nil {
		return splitAtomicText(block, cfg)
	}
	raw, _ := block.table.renderRows(block.table.rows)
	reserve := cfg.enhancementReserve()
	var result []semanticBlock
	emit := func(fragment string) {
		if fragment == "" {
			return
		}
		clone := block
		clone.table = nil
		clone.raw = wrapSemanticRaw(block, serializeCode(fragment, "table-gfm-fragment"))
		clone.bindings = nil
		result = append(result, clone)
	}

	buffer := ""
	for _, line := range splitCodeLines(raw) {
		candidate := buffer + line
		if candidate != "" && fitsBlock(block,
			wrapSemanticRaw(block, serializeCode(candidate, "table-gfm-fragment")), cfg, reserve) {
			buffer = candidate
			continue
		}
		emit(buffer)
		buffer = ""
		if fitsBlock(block, wrapSemanticRaw(block, serializeCode(line, "table-gfm-fragment")), cfg, reserve) {
			buffer = line
			continue
		}
		remaining := line
		for remaining != "" {
			prefix := longestPrefixThatFits(remaining, func(candidate string) bool {
				return fitsBlock(block,
					wrapSemanticRaw(block, serializeCode(candidate, "table-gfm-fragment")), cfg, reserve)
			})
			if prefix == "" {
				_, size := utf8.DecodeRuneInString(remaining)
				prefix = remaining[:size]
			}
			emit(prefix)
			remaining = remaining[len(prefix):]
		}
	}
	emit(buffer)
	if len(result) == 0 {
		emit(raw)
	}
	return localizeBindings(result, block.bindings)
}
