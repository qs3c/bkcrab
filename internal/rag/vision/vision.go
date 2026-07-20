// Package vision implements the ingestion-only DocumentAI boundary. Images
// enter this package only after bounded normalization; typed text leaves it.
// The package is deliberately independent from answer-model configuration.
package vision

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

const (
	PageSchemaVersion             = "page-transcription-v1"
	ImageDescriptionSchemaVersion = "image-description-v1"
	maxSourceLocationLabelBytes   = 1024
)

var (
	ErrInvalidResponse = errors.New("vision: invalid typed response")
	ErrBudgetRequired  = errors.New("vision: durable DocumentAI budget is required")
	ErrAttemptNotSent  = errors.New("vision: outbound attempt was not authorized to send")
	ErrCacheCommitted  = errors.New("vision: logical request was already committed")
)

// ErrorKind is intentionally small so the pipeline can apply its degradation
// matrix without parsing provider messages.
type ErrorKind string

const (
	ErrorInvalid   ErrorKind = "invalid_response"
	ErrorTimeout   ErrorKind = "timeout"
	ErrorRateLimit ErrorKind = "rate_limit"
	ErrorUpstream  ErrorKind = "upstream"
	ErrorPolicy    ErrorKind = "policy"
	ErrorBudget    ErrorKind = "budget"
)

type Error struct {
	Kind       ErrorKind
	StatusCode int
	Err        error
}

func (e *Error) Error() string {
	if e == nil {
		return "vision: unknown error"
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("vision: %s (HTTP %d): %v", e.Kind, e.StatusCode, e.Err)
	}
	return fmt.Sprintf("vision: %s: %v", e.Kind, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

func IsRetryable(err error) bool {
	var typed *Error
	if !errors.As(err, &typed) {
		return false
	}
	return typed.Kind == ErrorTimeout || typed.Kind == ErrorRateLimit || typed.Kind == ErrorUpstream
}

type CacheScope struct {
	UserID string
	KBID   string
	DocID  string
}

func (s CacheScope) empty() bool { return s.UserID == "" && s.KBID == "" && s.DocID == "" }

// NormalizedImageInput contains only safe raster bytes and non-secret source
// context. Scope is used solely for tenant-local object-cache addressing and
// is never serialized into an upstream request.
type NormalizedImageInput struct {
	Bytes       []byte
	MIMEType    string
	Width       int
	Height      int
	SHA256      string
	Base64Bytes int64

	Format   string
	Location document.SourceLocation
	AltText  string
	Scope    CacheScope
}

func (i NormalizedImageInput) Validate() error {
	if len(i.Bytes) == 0 || i.MIMEType != "image/jpeg" || i.Width <= 0 || i.Height <= 0 {
		return errors.New("vision: image is not a normalized JPEG")
	}
	if !document.CanonicalSHA256(i.SHA256) {
		return errors.New("vision: normalized image hash is invalid")
	}
	if i.Base64Bytes <= 0 {
		return errors.New("vision: normalized image base64 size is missing")
	}
	if i.Format != "" && !safeContextText(i.Format, 32) {
		return errors.New("vision: image format context is invalid")
	}
	if !safeContextText(i.Location.Label, maxSourceLocationLabelBytes) {
		return errors.New("vision: image location label is invalid")
	}
	if i.Location.Kind != "" {
		if err := i.Location.Validate(); err != nil {
			return fmt.Errorf("vision: image location: %w", err)
		}
	}
	if !safeContextText(i.AltText, 4096) {
		return errors.New("vision: image alt text is invalid")
	}
	return nil
}

type PageInput struct {
	Image NormalizedImageInput
}

type Visual struct {
	Key        string                  `json:"key"`
	Kind       string                  `json:"kind"`
	BBox       document.NormalizedBBox `json:"bbox"`
	Caption    string                  `json:"caption"`
	OCRText    string                  `json:"ocrText"`
	Decorative bool                    `json:"decorative"`
	Confidence float64                 `json:"confidence"`
}

type PageTranscription struct {
	Markdown string   `json:"markdown"`
	Visuals  []Visual `json:"visuals"`
}

type ImageDescription struct {
	Kind       string  `json:"kind"`
	Caption    string  `json:"caption"`
	OCRText    string  `json:"ocrText"`
	Decorative bool    `json:"decorative"`
	Confidence float64 `json:"confidence"`
}

// PageTranscriber and ImageTranscriber are the only parser-facing network
// boundaries. The same task-scoped budget instance must be passed to every
// page, image and repair request.
type PageTranscriber interface {
	TranscribePage(context.Context, PageInput, *TaskDocumentAIBudget) (PageTranscription, error)
}

type ImageTranscriber interface {
	DescribeImage(context.Context, NormalizedImageInput, *TaskDocumentAIBudget) (ImageDescription, error)
}

func validConfidence(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func safeContextText(value string, maxBytes int) bool {
	return len(value) <= maxBytes && strings.ToValidUTF8(value, "") == value && !strings.ContainsRune(value, 0)
}

var _ PageTranscriber = (*Client)(nil)
var _ ImageTranscriber = (*Client)(nil)
