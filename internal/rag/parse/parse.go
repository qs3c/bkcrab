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

// ErrEmptyContent covers empty files and documents with no extractable text,
// including scanned PDFs for which OCR is intentionally not performed.
var ErrEmptyContent = errors.New("文档无有效文本内容(扫描件或空文件)")

// SupportedExt reports whether fileName has a supported extension.
func SupportedExt(fileName string) bool {
	switch strings.ToLower(filepath.Ext(fileName)) {
	case ".md", ".markdown", ".txt", ".pdf", ".docx":
		return true
	default:
		return false
	}
}

// Parse reads and extracts a supported document.
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
		return nil, fmt.Errorf("不支持的文件类型 %q(支持 md/txt/pdf/docx)", ext)
	}

	for _, page := range result.Pages {
		text := strings.TrimPrefix(page.Text, "\ufeff")
		if strings.TrimSpace(text) != "" {
			return result, nil
		}
	}
	return nil, ErrEmptyContent
}
