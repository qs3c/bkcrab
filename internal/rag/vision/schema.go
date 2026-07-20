package vision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

type SchemaLimits struct {
	MaxJSONDepth        int
	MaxMarkdownBytes    int
	MaxVisuals          int
	MaxCaptionBytes     int
	MaxOCRBytes         int
	MaxDescriptionBytes int
}

func DefaultSchemaLimits() SchemaLimits {
	return SchemaLimits{
		MaxJSONDepth: 32, MaxMarkdownBytes: 256 << 10, MaxVisuals: 100,
		MaxCaptionBytes: 8 << 10, MaxOCRBytes: 64 << 10, MaxDescriptionBytes: 72 << 10,
	}
}

func (l SchemaLimits) normalized() SchemaLimits {
	d := DefaultSchemaLimits()
	if l.MaxJSONDepth <= 0 {
		l.MaxJSONDepth = d.MaxJSONDepth
	}
	if l.MaxMarkdownBytes <= 0 {
		l.MaxMarkdownBytes = d.MaxMarkdownBytes
	}
	if l.MaxVisuals <= 0 {
		l.MaxVisuals = d.MaxVisuals
	}
	if l.MaxCaptionBytes <= 0 {
		l.MaxCaptionBytes = d.MaxCaptionBytes
	}
	if l.MaxOCRBytes <= 0 {
		l.MaxOCRBytes = d.MaxOCRBytes
	}
	if l.MaxDescriptionBytes <= 0 {
		l.MaxDescriptionBytes = d.MaxDescriptionBytes
	}
	return l
}

var (
	visualKeyPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	visualMarkerPattern = regexp.MustCompile(`^rag-visual://([A-Za-z0-9][A-Za-z0-9._-]{0,63})$`)
	uriPattern          = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]{0,31}:[^\s<>"'()\[\]{}]+`)
	bareWWWPattern      = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9_-])www\.[A-Za-z0-9]`)
	uriAttributePattern = regexp.MustCompile(`(?i)\b(?:href|src|srcset|action|formaction|poster)\s*=`)
	cssURLPattern       = regexp.MustCompile(`(?i)\burl\s*\(`)
	longBase64Pattern   = regexp.MustCompile(`[A-Za-z0-9+/]{128,}={0,2}`)
)

var allowedVisualKinds = map[string]struct{}{
	"diagram": {}, "chart": {}, "table": {}, "code": {}, "photo": {},
	"illustration": {}, "screenshot": {}, "formula": {}, "other": {},
}

func DecodePageTranscription(raw []byte, limits SchemaLimits) (PageTranscription, error) {
	var value PageTranscription
	if err := strictDecode(raw, limits.normalized().MaxJSONDepth, &value); err != nil {
		return PageTranscription{}, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	if err := value.Validate(limits); err != nil {
		return PageTranscription{}, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return value, nil
}

func (p PageTranscription) Validate(limits SchemaLimits) error {
	limits = limits.normalized()
	if !utf8.ValidString(p.Markdown) || len(p.Markdown) > limits.MaxMarkdownBytes || strings.ContainsRune(p.Markdown, 0) {
		return errors.New("page markdown is invalid or too large")
	}
	if len(p.Visuals) > limits.MaxVisuals {
		return fmt.Errorf("too many visuals: %d", len(p.Visuals))
	}
	markerCount, err := pageMarkdownMarkers(p.Markdown)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(p.Visuals))
	for index := range p.Visuals {
		visual := &p.Visuals[index]
		if !visualKeyPattern.MatchString(visual.Key) {
			return fmt.Errorf("visual %d has invalid key", index)
		}
		if _, duplicate := seen[visual.Key]; duplicate {
			return fmt.Errorf("duplicate visual key %q", visual.Key)
		}
		seen[visual.Key] = struct{}{}
		if markerCount[visual.Key] != 1 {
			return fmt.Errorf("visual %q must be referenced exactly once", visual.Key)
		}
		if _, ok := allowedVisualKinds[visual.Kind]; !ok {
			return fmt.Errorf("visual %q has unsupported kind %q", visual.Key, visual.Kind)
		}
		if err := visual.BBox.Validate(); err != nil {
			return err
		}
		if err := validateDescriptionFields(visual.Caption, visual.OCRText, visual.Confidence, limits); err != nil {
			return fmt.Errorf("visual %q: %w", visual.Key, err)
		}
	}
	for key, count := range markerCount {
		if count != 1 {
			return fmt.Errorf("visual marker %q appears %d times", key, count)
		}
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("visual marker %q has no typed visual", key)
		}
	}
	return nil
}

func DecodeImageDescription(raw []byte, limits SchemaLimits) (ImageDescription, error) {
	var value ImageDescription
	if err := strictDecode(raw, limits.normalized().MaxJSONDepth, &value); err != nil {
		return ImageDescription{}, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	if err := value.Validate(limits); err != nil {
		return ImageDescription{}, fmt.Errorf("%w: %v", ErrInvalidResponse, err)
	}
	return value, nil
}

func (d ImageDescription) Validate(limits SchemaLimits) error {
	limits = limits.normalized()
	if _, ok := allowedVisualKinds[d.Kind]; !ok {
		return fmt.Errorf("unsupported image kind %q", d.Kind)
	}
	if err := validateDescriptionFields(d.Caption, d.OCRText, d.Confidence, limits); err != nil {
		return err
	}
	if !d.Decorative && strings.TrimSpace(d.Caption) == "" && strings.TrimSpace(d.OCRText) == "" {
		return errors.New("non-decorative image requires caption or OCR text")
	}
	return nil
}

func validateDescriptionFields(caption, ocr string, confidence float64, limits SchemaLimits) error {
	if !utf8.ValidString(caption) || !utf8.ValidString(ocr) || strings.ContainsRune(caption, 0) || strings.ContainsRune(ocr, 0) {
		return errors.New("caption/OCR must be valid UTF-8")
	}
	if len(caption) > limits.MaxCaptionBytes || len(ocr) > limits.MaxOCRBytes || len(caption)+len(ocr) > limits.MaxDescriptionBytes {
		return errors.New("caption/OCR exceeds schema byte limit")
	}
	if forbiddenContent(caption, false) || forbiddenContent(ocr, false) {
		return errors.New("caption/OCR contains a forbidden URL, scheme, or encoded payload")
	}
	if !validConfidence(confidence) {
		return errors.New("confidence must be between 0 and 1")
	}
	return nil
}

func forbiddenContent(value string, allowVisualMarker bool) bool {
	lower := strings.ToLower(value)
	if longBase64Pattern.MatchString(value) || strings.Contains(lower, "base64,") ||
		bareWWWPattern.MatchString(value) || uriAttributePattern.MatchString(value) ||
		cssURLPattern.MatchString(value) || containsProtocolRelativeURI(value) {
		return true
	}
	for _, candidate := range uriPattern.FindAllString(value, -1) {
		if allowVisualMarker && visualMarkerPattern.MatchString(candidate) {
			continue
		}
		return true
	}
	return false
}

func containsProtocolRelativeURI(value string) bool {
	for offset := 0; offset < len(value); {
		index := strings.Index(value[offset:], "//")
		if index < 0 {
			return false
		}
		index += offset
		start := index + 2
		if index > 0 && value[index-1] == ':' {
			offset = start
			continue
		}
		if index > 0 && !protocolRelativeBoundary(value[index-1]) {
			offset = start
			continue
		}
		end := start
		for end < len(value) && !strings.ContainsRune(" \t\r\n<>\"'(){}", rune(value[end])) {
			end++
		}
		if end > start {
			return true
		}
		offset = start
	}
	return false
}

func protocolRelativeBoundary(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n' ||
		strings.ContainsRune("([{'\"=<>`,", rune(value))
}

func pageMarkdownMarkers(markdown string) (map[string]int, error) {
	if forbiddenContent(markdown, true) {
		return nil, errors.New("page markdown contains a forbidden URI, scheme, or encoded payload")
	}
	source := []byte(markdown)
	root := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(text.NewReader(source))
	markers := make(map[string]int)
	err := ast.Walk(root, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch typed := node.(type) {
		case *ast.Image:
			match := visualMarkerPattern.FindStringSubmatch(string(typed.Destination))
			if match == nil {
				return ast.WalkStop, errors.New("page markdown image destination must be an exact canonical visual marker")
			}
			markers[match[1]]++
		case *ast.Link:
			return ast.WalkStop, errors.New("page markdown links are forbidden")
		case *ast.AutoLink:
			return ast.WalkStop, errors.New("page markdown autolinks are forbidden")
		}
		return ast.WalkContinue, nil
	})
	if err != nil {
		return nil, err
	}

	rawMarkers := make(map[string]int)
	for _, candidate := range uriPattern.FindAllString(markdown, -1) {
		if match := visualMarkerPattern.FindStringSubmatch(candidate); match != nil {
			rawMarkers[match[1]]++
		}
	}
	if len(rawMarkers) != len(markers) {
		return nil, errors.New("page markdown visual markers must occur only as image destinations")
	}
	for key, count := range rawMarkers {
		if markers[key] != count {
			return nil, errors.New("page markdown visual markers must occur only as image destinations")
		}
	}
	return markers, nil
}

func strictDecode(raw []byte, maxDepth int, value any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("empty JSON")
	}
	if err := validateJSONDepth(raw, maxDepth); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func validateJSONDepth(raw []byte, maxDepth int) error {
	if maxDepth <= 0 {
		return errors.New("invalid maximum JSON depth")
	}
	inString, escaped, depth := false, false, 0
	for _, b := range raw {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == '"' {
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return fmt.Errorf("JSON depth exceeds %d", maxDepth)
			}
		case '}', ']':
			depth--
			if depth < 0 {
				return errors.New("unbalanced JSON")
			}
		}
	}
	if inString || depth != 0 {
		return errors.New("incomplete JSON")
	}
	return nil
}
