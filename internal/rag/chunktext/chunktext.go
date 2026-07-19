// Package chunktext builds the text indexed for one document chunk while
// keeping the original body available to retrieval callers.
package chunktext

import "strings"

const sectionPrefix = "章节："

// Search joins a section title and body into the text used by embedding and
// full-text indexing. Chunks without a section title remain unchanged.
func Search(sectionTitle, content string) string {
	title := strings.TrimSpace(sectionTitle)
	if title == "" {
		return content
	}
	return sectionPrefix + title + "\n\n" + content
}

// ForIndex returns an explicitly prepared search text when present, otherwise
// it derives one from the full section title. The fallback keeps manually
// constructed ChunkData values and older callers structure-aware as well.
func ForIndex(searchContent, sectionTitle, content string) string {
	if searchContent != "" {
		return searchContent
	}
	return Search(sectionTitle, content)
}

// Body removes the section prefix written by Search. Existing Milvus rows that
// contain only the original body are returned unchanged, so old and new index
// versions can be read during a rolling reindex.
func Body(stored, sectionTitle string) string {
	title := strings.TrimSpace(sectionTitle)
	if title == "" || !strings.HasPrefix(stored, sectionPrefix) {
		return stored
	}
	separator := strings.Index(stored, "\n\n")
	if separator < 0 {
		return stored
	}
	indexedTitle := strings.TrimSpace(strings.TrimPrefix(stored[:separator], sectionPrefix))
	if indexedTitle == title {
		return stored[separator+2:]
	}
	// Very long breadcrumbs are shortened from the left before indexing so the
	// most specific heading remains searchable. Validate that suffix before
	// treating the stored value as the new envelope format.
	if strings.HasPrefix(indexedTitle, "…") &&
		strings.HasSuffix(title, strings.TrimPrefix(indexedTitle, "…")) {
		return stored[separator+2:]
	}
	return stored
}
