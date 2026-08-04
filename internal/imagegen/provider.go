package imagegen

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

type Capability struct {
	MaxImagesPerCall int      `json:"max_images_per_call"`
	SupportedSizes   []string `json:"supported_sizes"`
	SupportsSeed     bool     `json:"supports_seed"`
}

func (c Capability) Supports(size string, count int) bool {
	if count < 1 || c.MaxImagesPerCall < count {
		return false
	}
	for _, supported := range c.SupportedSizes {
		if supported == size {
			return true
		}
	}
	return false
}

type GenerateRequest struct {
	Prompt string
	Size   string
	Count  int
}

func (r GenerateRequest) Validate() error {
	if strings.TrimSpace(r.Prompt) == "" {
		return NewProviderError(ErrorInvalidRequest, "", 0, errors.New("prompt is required"))
	}
	if r.Count < 1 || r.Count > 4 {
		return NewProviderError(ErrorInvalidRequest, "", 0, errors.New("count must be in [1,4]"))
	}
	if r.Size != SizeSquare && r.Size != SizeLandscape && r.Size != SizePortrait {
		return NewProviderError(ErrorInvalidRequest, "", 0, errors.New("unsupported size"))
	}
	return nil
}

type GeneratedImage struct {
	Bytes     []byte
	SourceURL string
	MIMEType  string
	Width     int
	Height    int
}

type AttemptSummary struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model,omitempty"`
	Kind     ErrorKind `json:"kind"`
	Message  string    `json:"message,omitempty"`
}

type ProviderResult struct {
	Images   []GeneratedImage
	Provider string
	Model    string
	Attempts []AttemptSummary
}

type Backend interface {
	Name() string
	Capability(model string) Capability
	Generate(ctx context.Context, cfg ResolvedProviderConfig, req GenerateRequest) (ProviderResult, error)
}

type ErrorKind string

const (
	ErrorInvalidRequest    ErrorKind = "INVALID_REQUEST"
	ErrorSafetyRejected    ErrorKind = "SAFETY_REJECTED"
	ErrorAuthConfig        ErrorKind = "AUTH_CONFIG"
	ErrorRateLimited       ErrorKind = "RATE_LIMITED"
	ErrorUpstreamTransient ErrorKind = "UPSTREAM_TRANSIENT"
	ErrorModelUnavailable  ErrorKind = "MODEL_UNAVAILABLE"
	ErrorEmptyResult       ErrorKind = "EMPTY_RESULT"
	ErrorIncompleteResult  ErrorKind = "INCOMPLETE_RESULT"
	ErrorMalformedResult   ErrorKind = "MALFORMED_RESULT"
)

type ProviderError struct {
	Kind       ErrorKind
	Provider   string
	StatusCode int
	Err        error
}

func NewProviderError(kind ErrorKind, provider string, statusCode int, err error) error {
	return &ProviderError{Kind: kind, Provider: provider, StatusCode: statusCode, Err: err}
}

func (e *ProviderError) Error() string {
	if e == nil {
		return "imagegen: provider error"
	}
	if e.Provider != "" {
		return fmt.Sprintf("imagegen: provider %s failed (%s)", e.Provider, e.Kind)
	}
	return fmt.Sprintf("imagegen: provider request failed (%s)", e.Kind)
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func ProviderErrorKind(err error) ErrorKind {
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		return providerErr.Kind
	}
	return ""
}
