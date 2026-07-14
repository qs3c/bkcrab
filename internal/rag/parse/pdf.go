package parse

import (
	"bytes"
	"fmt"

	pdflib "github.com/ledongthuc/pdf"
)

// extractPDF extracts each page's text layer. OCR is intentionally out of
// scope; Parse reports ErrEmptyContent when no page contains extractable text.
func extractPDF(data []byte) ([]Page, error) {
	reader, err := pdflib.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("pdf 解析: %w", err)
	}
	pages := make([]Page, 0, reader.NumPage())
	for number := 1; number <= reader.NumPage(); number++ {
		page := reader.Page(number)
		if page.V.IsNull() {
			continue
		}
		text, pageErr := page.GetPlainText(nil)
		if pageErr != nil {
			continue
		}
		pages = append(pages, Page{Num: number, Text: text})
	}
	return pages, nil
}
