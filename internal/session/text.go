package session

// Slice Unicode code points rather than bytes so Chinese/emoji previews remain
// valid UTF-8. Custom user titles are never truncated in storage.
func truncateSessionText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}
