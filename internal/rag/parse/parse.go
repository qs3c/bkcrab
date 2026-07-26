// Package parse converts supported uploaded documents into page-oriented text.
// Markdown is kept intact so the splitter can use its heading structure.
package parse

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Page contains text extracted from one source page.
type Page struct {
	Num  int
	Text string
}

// Result is the normalized representation of a parsed document.
type Result struct {
	Format string // md / txt / pdf / docx
	Pages  []Page // md, txt, and docx have one logical page
}

var (
	// ErrEmptyContent covers empty files and documents with no extractable text,
	// including scanned PDFs for which OCR is intentionally not performed.
	ErrEmptyContent = errors.New("文档无有效文本内容(扫描件或空文件)")
	// ErrInvalidDocument marks deterministic container/content failures that a
	// task retry cannot repair.
	ErrInvalidDocument = errors.New("invalid document content")
	// ErrDocumentLimitExceeded marks a configured structural hard limit.
	ErrDocumentLimitExceeded = errors.New("document hard limit exceeded")
	// ErrSourceIntegrity marks a mismatch between an immutable source snapshot
	// and the bytes returned by its reopen function.
	ErrSourceIntegrity = errors.New("document source integrity mismatch")
)

// SupportedExt reports whether fileName has a supported extension.
func SupportedExt(fileName string) bool {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".md", ".markdown", ".txt", ".pdf", ".docx", ".pptx", ".xlsx":
		return true
	default:
		return false
	}
}

// Parse reads and extracts documents supported by the original parser.
//
// Deprecated: this compatibility wrapper is retained for legacy tests. New
// indexing code must use Parser so PDF sources remain streaming and Office
// documents cannot silently fall back to the old DOCX XML extractor.
func Parse(r io.Reader, fileName string) (*Result, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("读取文件: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(fileName))
	var result *Result
	switch ext {
	case ".md", ".markdown":
		result = &Result{Format: "md", Pages: []Page{{Num: 1, Text: string(data)}}}
	case ".txt":
		result = &Result{Format: "txt", Pages: []Page{{Num: 1, Text: string(data)}}}
	case ".docx":
		text, extractErr := extractDocx(data)
		if extractErr != nil {
			return nil, extractErr
		}
		result = &Result{Format: "docx", Pages: []Page{{Num: 1, Text: text}}}
	case ".pdf":
		pages, extractErr := extractPDF(data)
		if extractErr != nil {
			return nil, extractErr
		}
		result = &Result{Format: "pdf", Pages: pages}
	default:
		return nil, fmt.Errorf("旧解析器不支持文件类型 %q(仅支持 md/txt/pdf/docx)", ext)
	}

	for _, page := range result.Pages {
		text := strings.TrimPrefix(page.Text, "\ufeff")
		if strings.TrimSpace(text) != "" {
			return result, nil
		}
	}
	return nil, ErrEmptyContent
}
