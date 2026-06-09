package channels

import (
	"context"
	"strings"

	"github.com/bkclaw-ai/bkclaw/internal/bus"
)

// SplitMessageMarker is the on-the-wire control token the LLM emits to
// ask an IM-style adapter (WeChat, …) to split a single outbound text
// payload into multiple separate chat bubbles. We picked a token that
//
//  1. won't appear in natural prose, so the agent can't trigger a split
//     by accident in markdown / code / quoted text;
//  2. survives WeChat's wechatStripMarkdown pass — it's not parsed as
//     any markdown construct;
//  3. reads as "control instruction" both to a human inspecting the
//     transcript and to the LLM emitting it.
//
// The agent-side hint that introduces this token to the model lives in
// internal/agent/loop.go under the per-turn system-prompt addendum, so
// the protocol stays advertised in exactly one place.
const SplitMessageMarker = "<|split|>"

// FlattenMarkdownTables converts every GFM-style table block in `text`
// into a flat, no-syntax form that IM channels actually render. None of
// the IM platforms we support (Discord, Telegram, LINE, Slack, Feishu,
// WeChat) render markdown tables — they ship them as raw `|cell|cell|`
// rows with a `|---|---|` separator line right in the middle, which
// looks like a malfunction to the chatter.
//
// Detection is GFM-strict: a table is two-or-more consecutive lines
// where the first is a header row, the second is the separator
// (`|---|...` with optional alignment colons), and everything after is
// data until we hit a non-table line. Anything that doesn't match
// passes through byte-for-byte — quoted text containing pipes, code
// fences, accidental "|" in prose all survive.
//
// Output shape:
//
//   2-column tables  → "header1: header2" line, then one
//                      "cell1: cell2" line per row. This is the most
//                      common shape LLMs emit (label / value lists)
//                      and reads cleanly as plain text.
//   3+ column tables → cells joined with " · " (middle dot) per row,
//                      no separator. Loses alignment but stays on one
//                      line per row and scans as tabular at a glance.
//
// Cells are trimmed; the GFM escape `\|` round-trips back to a literal
// `|` inside a cell. The separator row is dropped in every shape.
func FlattenMarkdownTables(text string) string {
	if !strings.Contains(text, "|") {
		return text
	}
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	for i := 0; i < len(lines); {
		// A table starts when we see a row immediately followed by a
		// separator row. Anything else — even a stray pipe-bearing
		// line — passes through.
		if i+1 < len(lines) && isMarkdownTableRow(lines[i]) && isMarkdownTableSeparator(lines[i+1]) {
			header := parseMarkdownTableRow(lines[i])
			i += 2 // consume header + separator
			rows := [][]string{header}
			for i < len(lines) && isMarkdownTableRow(lines[i]) {
				rows = append(rows, parseMarkdownTableRow(lines[i]))
				i++
			}
			out = append(out, renderFlatTable(rows))
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n")
}

// isMarkdownTableRow returns true when the trimmed line begins AND ends
// with an unescaped `|` AND contains at least one more `|` between
// them — the GFM table-row shape. A bare "|" or a single field doesn't
// count; the false-positive cost on prose lines is otherwise too high.
func isMarkdownTableRow(line string) bool {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) < 3 || trimmed[0] != '|' || trimmed[len(trimmed)-1] != '|' {
		return false
	}
	// Need an interior pipe — count unescaped ones inside the bounds.
	interior := trimmed[1 : len(trimmed)-1]
	for i := 0; i < len(interior); i++ {
		if interior[i] == '|' && (i == 0 || interior[i-1] != '\\') {
			return true
		}
	}
	return false
}

// isMarkdownTableSeparator returns true for a GFM table separator line
// — a row where every cell matches `^\s*:?-+:?\s*$`. Tolerates an empty
// pipe-only line for the same reason GFM does (some emitters skip the
// dashes inside an empty column).
func isMarkdownTableSeparator(line string) bool {
	if !isMarkdownTableRow(line) {
		return false
	}
	cells := parseMarkdownTableRow(line)
	if len(cells) == 0 {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		c = strings.TrimPrefix(c, ":")
		c = strings.TrimSuffix(c, ":")
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' {
				return false
			}
		}
	}
	return true
}

// parseMarkdownTableRow splits one table row into trimmed cells. Honors
// GFM's `\|` escape so a cell containing a literal pipe round-trips.
func parseMarkdownTableRow(line string) []string {
	trimmed := strings.TrimSpace(line)
	// Drop the leading/trailing pipes; without that the split produces
	// a phantom empty cell at each end.
	if strings.HasPrefix(trimmed, "|") {
		trimmed = trimmed[1:]
	}
	if strings.HasSuffix(trimmed, "|") {
		trimmed = trimmed[:len(trimmed)-1]
	}
	var cells []string
	var cur strings.Builder
	for i := 0; i < len(trimmed); i++ {
		c := trimmed[i]
		if c == '\\' && i+1 < len(trimmed) && trimmed[i+1] == '|' {
			cur.WriteByte('|')
			i++
			continue
		}
		if c == '|' {
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	cells = append(cells, strings.TrimSpace(cur.String()))
	return cells
}

// renderFlatTable formats the parsed rows for plain-text channels.
// rows[0] is the header. See FlattenMarkdownTables for shape rules.
func renderFlatTable(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	var b strings.Builder
	if cols == 2 {
		for i, r := range rows {
			left := ""
			right := ""
			if len(r) > 0 {
				left = r[0]
			}
			if len(r) > 1 {
				right = r[1]
			}
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(left)
			b.WriteString(": ")
			b.WriteString(right)
		}
		return b.String()
	}
	for i, r := range rows {
		if i > 0 {
			b.WriteByte('\n')
		}
		// Pad short rows to keep column alignment readable; missing
		// cells appear as empty strings between dots.
		padded := r
		for len(padded) < cols {
			padded = append(padded, "")
		}
		b.WriteString(strings.Join(padded, " · "))
	}
	return b.String()
}

// SplitOutboundText splits a reply payload on SplitMessageMarker into
// one chunk per bubble the adapter should send. Trims whitespace on each
// chunk and drops empties so a trailing marker or accidental double-
// split doesn't produce a blank message. Returns a single-element slice
// for the common case where the agent didn't ask to split — adapters
// can call this unconditionally without a branch.
func SplitOutboundText(text string) []string {
	parts := strings.Split(text, SplitMessageMarker)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Channel is the interface that all channel implementations must satisfy.
type Channel interface {
	// Name returns the channel type identifier (e.g. "telegram").
	Name() string
	// AccountID returns the account identifier within the channel.
	AccountID() string
	// BotUsername returns the bot's username for this channel (e.g. "mike_bkclaw_bot").
	// Returns empty string if not applicable.
	BotUsername() string
	// Start begins listening for messages. It should block until ctx is cancelled.
	Start(ctx context.Context) error
	// Send sends a plain text message to the specified chat.
	Send(chatID string, text string) error
	// SendMessage sends a rich outbound message with formatting, reply-to, buttons, etc.
	SendMessage(msg bus.OutboundMessage) error
	// SendTyping sends a typing indicator to the specified chat.
	SendTyping(chatID string) error
}
