package parse

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

const maxDOCXXMLBytes = 32 << 20

// extractDocx reads word/document.xml and emits one Markdown-like paragraph
// stream. HeadingN paragraph styles become ATX headings for the downstream
// structure-aware splitter.
func extractDocx(data []byte) (string, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("docx 不是合法 zip: %w", err)
	}
	var document io.ReadCloser
	for _, file := range archive.File {
		if file.Name != "word/document.xml" {
			continue
		}
		if file.UncompressedSize64 > maxDOCXXMLBytes {
			return "", fmt.Errorf("docx document.xml 解压后超过 %dMB 限制", maxDOCXXMLBytes>>20)
		}
		document, err = file.Open()
		if err != nil {
			return "", fmt.Errorf("打开 docx document.xml: %w", err)
		}
		break
	}
	if document == nil {
		return "", fmt.Errorf("docx 缺少 word/document.xml")
	}
	defer document.Close()

	// A small uploaded DOCX can otherwise contain a highly compressed XML zip
	// bomb. Keep a second streaming limit even after checking the central
	// directory''s declared uncompressed size.
	limited := &io.LimitedReader{R: document, N: maxDOCXXMLBytes + 1}
	decoder := xml.NewDecoder(limited)
	var output strings.Builder
	var paragraph strings.Builder
	headingLevel := 0
	inParagraph := false
	for {
		token, tokenErr := decoder.Token()
		if tokenErr == io.EOF {
			if limited.N <= 0 {
				return "", fmt.Errorf("docx document.xml 解压后超过 %dMB 限制", maxDOCXXMLBytes>>20)
			}
			break
		}
		if tokenErr != nil {
			return "", fmt.Errorf("docx xml 解析: %w", tokenErr)
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "p":
				paragraph.Reset()
				headingLevel = 0
				inParagraph = true
			case "pStyle":
				for _, attr := range element.Attr {
					if attr.Name.Local == "val" {
						headingLevel = headingStyleLevel(attr.Value)
						break
					}
				}
			case "t":
				if !inParagraph {
					continue
				}
				var text string
				if err := decoder.DecodeElement(&text, &element); err != nil {
					return "", fmt.Errorf("docx 文本解析: %w", err)
				}
				paragraph.WriteString(text)
				if paragraph.Len()+output.Len() > maxDOCXXMLBytes {
					return "", fmt.Errorf("docx 文本超过 %dMB 限制", maxDOCXXMLBytes>>20)
				}
			case "tab":
				if inParagraph {
					paragraph.WriteByte('\t')
				}
			case "br", "cr":
				if inParagraph {
					paragraph.WriteByte('\n')
				}
			}
		case xml.EndElement:
			if element.Name.Local != "p" || !inParagraph {
				continue
			}
			inParagraph = false
			line := strings.TrimSpace(paragraph.String())
			if line == "" {
				continue
			}
			if headingLevel > 0 {
				output.WriteString(strings.Repeat("#", headingLevel))
				output.WriteByte(' ')
			}
			output.WriteString(line)
			output.WriteString("\n\n")
		}
	}
	return output.String(), nil
}

// headingStyleLevel recognizes Heading1, "heading 2", and numeric style IDs.
func headingStyleLevel(styleValue string) int {
	style := strings.ToLower(strings.TrimSpace(styleValue))
	style = strings.ReplaceAll(style, " ", "")
	style = strings.TrimPrefix(style, "heading")
	style = strings.TrimPrefix(style, "标题")
	if len(style) == 1 && style[0] >= '1' && style[0] <= '6' {
		return int(style[0] - '0')
	}
	return 0
}
