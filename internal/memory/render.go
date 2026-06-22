package memory

import "strings"

// RenderForPrompt parses stored USER.md / MEMORY.md bytes into entries, strips managed
// storage encoding, applies strict scan sanitization, then joins entries with blank lines.
// Blank/nil input returns "". It must not touch any Store.
func RenderForPrompt(target Target, data []byte) string {
	entries, _ := parseEntries(target, data)
	return strings.Join(safeEntriesForList(target, entries), "\n\n")
}
