package sidecar

import (
	"errors"
	"fmt"
)

var (
	// ErrCapabilityUnavailable means the configured sidecar cannot currently
	// serve the requested protocol capability. Callers may use a format-specific
	// fallback, but must not treat it as a malformed document.
	ErrCapabilityUnavailable = errors.New("rag parser capability unavailable")
	// ErrInvalidBundle identifies an untrusted sidecar response that violates
	// the versioned manifest or tar contract.
	ErrInvalidBundle = errors.New("invalid rag parser bundle")
	// ErrBundleLimitExceeded identifies a response that exceeds a local hard
	// limit. It remains distinct from ordinary protocol corruption for metrics.
	ErrBundleLimitExceeded = errors.New("rag parser bundle limit exceeded")
	// ErrSourceLimitExceeded is returned before network I/O when the immutable
	// source size exceeds the effective local/sidecar health limit.
	ErrSourceLimitExceeded = errors.New("rag parser source limit exceeded")
	// ErrSourceIntegrity marks a mismatch between the immutable source metadata
	// and bytes returned by its reopen function.
	ErrSourceIntegrity = errors.New("rag parser source integrity mismatch")
)

// HTTPError preserves a sidecar status code for the pipeline retry classifier.
// Response bodies are deliberately not retained because they may contain
// untrusted parser diagnostics or source material.
type HTTPError struct {
	Operation  string
	StatusCode int
}

func (e *HTTPError) Error() string {
	if e == nil {
		return "rag parser HTTP error"
	}
	return fmt.Sprintf("rag parser %s returned HTTP %d", e.Operation, e.StatusCode)
}

func (e *HTTPError) HTTPStatus() int {
	if e == nil {
		return 0
	}
	return e.StatusCode
}

type capabilityError struct {
	Capability string
	Reason     string
}

func (e *capabilityError) Error() string {
	if e == nil {
		return ErrCapabilityUnavailable.Error()
	}
	if e.Reason == "" {
		return fmt.Sprintf("%s: %s", ErrCapabilityUnavailable, e.Capability)
	}
	return fmt.Sprintf("%s: %s (%s)", ErrCapabilityUnavailable, e.Capability, e.Reason)
}

func (e *capabilityError) Unwrap() error { return ErrCapabilityUnavailable }

func unavailable(capability, reason string) error {
	return &capabilityError{Capability: capability, Reason: reason}
}
