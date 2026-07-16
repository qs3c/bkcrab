package rag

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/rag/vector"
	"github.com/qs3c/bkcrab/internal/store"
)

const (
	metadataMaxDocuments    = 12
	metadataMaxCatalogChars = 6000
	metadataMaxContentChars = 30000
	metadataMaxCharsPerDoc  = 6000
	metadataPrimaryDocCount = 4
	metadataRecentDocCount  = 4
)

// ErrNoReadyDocuments means a knowledge base has no fully indexed document
// content that an Agent can currently retrieve.
var ErrNoReadyDocuments = errors.New("知识库暂无处理完成的文档")

// MetadataSource is the bounded, deterministic document context sent to the
// LLM when generating a knowledge base name and description.
type MetadataSource struct {
	Catalog              string
	Excerpts             string
	DocumentCount        int
	SampledDocumentCount int
}

// BuildMetadataSource samples only DONE documents. DONE is deliberately the
// boundary: it means parsing, splitting, embedding, and vector upsert all
// succeeded, so generated metadata describes content Agents can actually use.
func (s *Service) BuildMetadataSource(ctx context.Context, ownerID, kbID string) (*MetadataSource, error) {
	if _, err := s.GetKB(ctx, ownerID, kbID); err != nil {
		return nil, err
	}
	documents, err := s.st.ListRAGDocumentsByKB(ctx, kbID)
	if err != nil {
		return nil, err
	}
	ready := make([]store.RAGDocumentRecord, 0, len(documents))
	for _, document := range documents {
		if strings.EqualFold(document.Status, "DONE") && document.ChunkCount > 0 {
			ready = append(ready, document)
		}
	}
	if len(ready) == 0 {
		return nil, ErrNoReadyDocuments
	}

	selected := selectMetadataDocuments(ready, metadataMaxDocuments)
	refs := make([]vector.ChunkRef, 0, len(selected)*3)
	for _, document := range selected {
		for _, index := range metadataChunkIndexes(document.ChunkCount) {
			refs = append(refs, vector.ChunkRef{
				DocID: document.ID, Index: index, DocVersion: document.Version,
			})
		}
	}
	chunks, err := s.vec.GetChunks(ctx, kbID, refs)
	if err != nil {
		return nil, fmt.Errorf("读取知识库分块: %w", err)
	}
	byRef := make(map[vector.ChunkRef]vector.ChunkData, len(chunks))
	for _, chunk := range chunks {
		byRef[vector.ChunkRef{
			DocID: chunk.DocID, Index: chunk.Index, DocVersion: chunk.DocVersion,
		}] = chunk
	}

	perDocumentBudget := min(metadataMaxCharsPerDoc, metadataMaxContentChars/len(selected))
	var excerpts strings.Builder
	writtenContent := 0
	sampledDocumentCount := 0
	for _, document := range selected {
		indexes := metadataChunkIndexes(document.ChunkCount)
		available := make([]vector.ChunkData, 0, len(indexes))
		for _, index := range indexes {
			if chunk, ok := byRef[vector.ChunkRef{
				DocID: document.ID, Index: index, DocVersion: document.Version,
			}]; ok && strings.TrimSpace(chunk.Content) != "" {
				available = append(available, chunk)
			}
		}
		if len(available) == 0 {
			continue
		}
		sampledDocumentCount++
		fmt.Fprintf(&excerpts, "\n## %s\n", document.FileName)
		chunkBudget := max(1, perDocumentBudget/len(available))
		for _, chunk := range available {
			remaining := metadataMaxContentChars - writtenContent
			if remaining <= 0 {
				break
			}
			budget := min(chunkBudget, remaining)
			content := truncateMetadataRunes(strings.TrimSpace(chunk.Content), budget)
			if content == "" {
				continue
			}
			location := fmt.Sprintf("分块 %d", chunk.Index+1)
			if chunk.SectionTitle != "" {
				location += "，章节：" + chunk.SectionTitle
			}
			if chunk.PageNum > 0 {
				location += fmt.Sprintf("，第 %d 页", chunk.PageNum)
			}
			fmt.Fprintf(&excerpts, "[%s]\n%s\n", location, content)
			writtenContent += utf8.RuneCountInString(content)
		}
	}
	if writtenContent == 0 {
		return nil, errors.New("已完成文档的索引分块暂时不可用，请稍后重试")
	}

	return &MetadataSource{
		Catalog:              metadataDocumentCatalog(ready),
		Excerpts:             strings.TrimSpace(excerpts.String()),
		DocumentCount:        len(ready),
		SampledDocumentCount: sampledDocumentCount,
	}, nil
}

// selectMetadataDocuments implements the agreed first-version policy:
// largest four, newest four, then a filename-sorted uniform sample. Duplicate
// documents between groups are skipped and each group scans farther to fill
// its allocation where possible.
func selectMetadataDocuments(documents []store.RAGDocumentRecord, limit int) []store.RAGDocumentRecord {
	if limit <= 0 || len(documents) == 0 {
		return nil
	}
	if len(documents) <= limit {
		return append([]store.RAGDocumentRecord(nil), documents...)
	}

	seen := make(map[string]bool, limit)
	selected := make([]store.RAGDocumentRecord, 0, limit)
	add := func(document store.RAGDocumentRecord) bool {
		if seen[document.ID] || len(selected) >= limit {
			return false
		}
		seen[document.ID] = true
		selected = append(selected, document)
		return true
	}
	addGroup := func(candidates []store.RAGDocumentRecord, count int) {
		added := 0
		for _, document := range candidates {
			if add(document) {
				added++
				if added == count {
					return
				}
			}
		}
	}

	largest := append([]store.RAGDocumentRecord(nil), documents...)
	sort.SliceStable(largest, func(i, j int) bool {
		if largest[i].TokenCount != largest[j].TokenCount {
			return largest[i].TokenCount > largest[j].TokenCount
		}
		return largest[i].ID < largest[j].ID
	})
	addGroup(largest, min(metadataPrimaryDocCount, limit))

	newest := append([]store.RAGDocumentRecord(nil), documents...)
	sort.SliceStable(newest, func(i, j int) bool {
		if !newest[i].UploadedAt.Equal(newest[j].UploadedAt) {
			return newest[i].UploadedAt.After(newest[j].UploadedAt)
		}
		return newest[i].ID < newest[j].ID
	})
	addGroup(newest, min(metadataRecentDocCount, limit-len(selected)))

	remaining := make([]store.RAGDocumentRecord, 0, len(documents)-len(selected))
	for _, document := range documents {
		if !seen[document.ID] {
			remaining = append(remaining, document)
		}
	}
	sort.SliceStable(remaining, func(i, j int) bool {
		left, right := strings.ToLower(remaining[i].FileName), strings.ToLower(remaining[j].FileName)
		if left != right {
			return left < right
		}
		return remaining[i].ID < remaining[j].ID
	})
	for _, index := range evenlySpacedIndexes(len(remaining), limit-len(selected)) {
		add(remaining[index])
	}
	for _, document := range remaining {
		add(document)
	}
	return selected
}

func evenlySpacedIndexes(length, count int) []int {
	if length <= 0 || count <= 0 {
		return nil
	}
	count = min(count, length)
	if count == 1 {
		return []int{length / 2}
	}
	indexes := make([]int, count)
	for i := range count {
		indexes[i] = i * (length - 1) / (count - 1)
	}
	return indexes
}

func metadataChunkIndexes(chunkCount int) []int {
	if chunkCount <= 0 {
		return nil
	}
	candidates := []int{0, chunkCount / 2, chunkCount - 1}
	indexes := make([]int, 0, 3)
	seen := map[int]bool{}
	for _, index := range candidates {
		if !seen[index] {
			seen[index] = true
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)
	return indexes
}

func metadataDocumentCatalog(documents []store.RAGDocumentRecord) string {
	var catalog strings.Builder
	for index, document := range documents {
		line := fmt.Sprintf("- %s（%d 个分块）\n", document.FileName, document.ChunkCount)
		remaining := metadataMaxCatalogChars - utf8.RuneCountInString(catalog.String())
		if remaining <= 0 {
			break
		}
		if utf8.RuneCountInString(line) > remaining {
			omitted := len(documents) - index
			catalog.WriteString(truncateMetadataRunes(fmt.Sprintf("- 另有 %d 篇文档名称省略", omitted), remaining))
			break
		}
		catalog.WriteString(line)
	}
	return strings.TrimSpace(catalog.String())
}

func truncateMetadataRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	if limit == 1 {
		return "…"
	}
	return string(runes[:limit-1]) + "…"
}
