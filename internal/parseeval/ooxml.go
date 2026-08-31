package parseeval

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

type ooxmlLimits struct {
	MaxEntries                int
	MaxEntryUncompressedBytes uint64
	MaxTotalUncompressedBytes uint64
	MaxCompressionRatio       float64
}

func defaultOOXMLLimits() ooxmlLimits {
	return ooxmlLimits{
		MaxEntries:                10_000,
		MaxEntryUncompressedBytes: 100 << 20,
		MaxTotalUncompressedBytes: 500 << 20,
		MaxCompressionRatio:       200,
	}
}

func validateOOXML(readerAt io.ReaderAt, size int64, format Format) error {
	if size <= 0 || !format.Valid() {
		return errors.New("invalid OOXML preflight input")
	}
	archive, err := zip.NewReader(readerAt, size)
	if err != nil {
		return fmt.Errorf("invalid OOXML ZIP: %w", err)
	}
	return inspectOOXMLArchive(archive, format, defaultOOXMLLimits())
}

func validateOOXMLBytes(data []byte, format Format, limits ooxmlLimits) error {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("invalid OOXML ZIP: %w", err)
	}
	return inspectOOXMLArchive(archive, format, limits)
}

func inspectOOXMLArchive(archive *zip.Reader, format Format, limits ooxmlLimits) error {
	if archive == nil || !format.Valid() || limits.MaxEntries <= 0 || limits.MaxEntryUncompressedBytes == 0 ||
		limits.MaxTotalUncompressedBytes == 0 || limits.MaxCompressionRatio <= 0 {
		return errors.New("invalid OOXML inspection limits")
	}
	if len(archive.File) == 0 || len(archive.File) > limits.MaxEntries {
		return fmt.Errorf("OOXML entry count exceeds %d", limits.MaxEntries)
	}
	requiredPart := map[Format]string{
		FormatDOCX: "word/document.xml",
		FormatPPTX: "ppt/presentation.xml",
		FormatXLSX: "xl/workbook.xml",
	}[format]
	var contentTypesEntry *zip.File
	var requiredEntry *zip.File
	var total uint64
	for _, entry := range archive.File {
		if err := validateArchiveMemberName(entry.Name); err != nil {
			return err
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		uncompressed := entry.UncompressedSize64
		if uncompressed > limits.MaxEntryUncompressedBytes || uncompressed > limits.MaxTotalUncompressedBytes ||
			total > limits.MaxTotalUncompressedBytes-uncompressed {
			return errors.New("OOXML expanded data exceeds limit")
		}
		total += uncompressed
		if uncompressed > 0 {
			if entry.CompressedSize64 == 0 || float64(uncompressed)/float64(entry.CompressedSize64) > limits.MaxCompressionRatio {
				return errors.New("OOXML compression ratio exceeds limit")
			}
		}
		switch entry.Name {
		case "[Content_Types].xml":
			contentTypesEntry = entry
		case requiredPart:
			requiredEntry = entry
		}
	}
	if contentTypesEntry == nil || requiredEntry == nil {
		return fmt.Errorf("OOXML package does not contain required %s parts", format)
	}
	if err := validateXMLArchivePart(contentTypesEntry); err != nil {
		return fmt.Errorf("invalid [Content_Types].xml: %w", err)
	}
	if err := validateXMLArchivePart(requiredEntry); err != nil {
		return fmt.Errorf("invalid %s: %w", requiredPart, err)
	}
	return nil
}

func validateArchiveMemberName(name string) error {
	if name == "" || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || path.IsAbs(name) ||
		strings.Contains(strings.SplitN(name, "/", 2)[0], ":") {
		return errors.New("OOXML archive contains an unsafe member name")
	}
	for _, segment := range strings.Split(name, "/") {
		if segment == ".." || segment == "." || segment == "" && !strings.HasSuffix(name, "/") {
			return errors.New("OOXML archive contains an unsafe member name")
		}
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != strings.TrimSuffix(name, "/") {
		return errors.New("OOXML archive contains an unsafe member name")
	}
	return nil
}

func validateXMLArchivePart(entry *zip.File) error {
	reader, err := entry.Open()
	if err != nil {
		return err
	}
	defer reader.Close()
	decoder := xml.NewDecoder(reader)
	foundRoot := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			if !foundRoot {
				return errors.New("XML part has no root element")
			}
			return nil
		}
		if err != nil {
			return err
		}
		if _, ok := token.(xml.StartElement); ok {
			foundRoot = true
		}
	}
}
