package parse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

const sourceCopyBufferBytes = 64 * 1024

func readSourceBytes(ctx context.Context, source document.Source) (content []byte, resultErr error) {
	if err := source.Validate(); err != nil {
		return nil, err
	}
	reader, err := source.Open(ctx)
	if err != nil {
		return nil, fmt.Errorf("open document source: %w", err)
	}
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("close document source: %w", closeErr))
		}
	}()

	var output bytes.Buffer
	hasher := sha256.New()
	written, err := copyExactSource(ctx, io.MultiWriter(&output, hasher), reader, source.Size)
	if err != nil {
		return nil, err
	}
	if written != source.Size {
		return nil, fmt.Errorf("%w: read %d bytes, expected %d", ErrSourceIntegrity, written, source.Size)
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != source.SHA256 {
		return nil, fmt.Errorf("%w: SHA-256 changed", ErrSourceIntegrity)
	}
	return output.Bytes(), nil
}

func spoolSourceFile(ctx context.Context, source document.Source, tempDir string) (*os.File, func() error, error) {
	if err := source.Validate(); err != nil {
		return nil, nil, err
	}
	temporary, err := os.CreateTemp(tempDir, "bkcrab-rag-source-*")
	if err != nil {
		return nil, nil, redactFileOperationError("create source spool", err)
	}
	name := temporary.Name()
	var cleanupOnce sync.Once
	var cleanupErr error
	cleanup := func() error {
		cleanupOnce.Do(func() {
			if closeErr := temporary.Close(); closeErr != nil && !errors.Is(closeErr, os.ErrClosed) {
				cleanupErr = errors.Join(cleanupErr, redactFileOperationError("close source spool", closeErr))
			}
			if removeErr := os.Remove(name); removeErr != nil && !os.IsNotExist(removeErr) {
				cleanupErr = errors.Join(cleanupErr, redactFileOperationError("remove source spool", removeErr))
			}
		})
		return cleanupErr
	}
	fail := func(cause error) (*os.File, func() error, error) {
		return nil, nil, errors.Join(cause, cleanup())
	}

	reader, err := source.Open(ctx)
	if err != nil {
		return fail(fmt.Errorf("open document source: %w", err))
	}
	hasher := sha256.New()
	written, copyErr := copyExactSource(ctx, io.MultiWriter(temporary, hasher), reader, source.Size)
	closeErr := reader.Close()
	if copyErr != nil || closeErr != nil {
		if closeErr != nil {
			closeErr = fmt.Errorf("close document source: %w", closeErr)
		}
		return fail(errors.Join(copyErr, closeErr))
	}
	if written != source.Size {
		return fail(fmt.Errorf("%w: read %d bytes, expected %d", ErrSourceIntegrity, written, source.Size))
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != source.SHA256 {
		return fail(fmt.Errorf("%w: SHA-256 changed", ErrSourceIntegrity))
	}
	if err := temporary.Sync(); err != nil {
		return fail(redactFileOperationError("sync source spool", err))
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		return fail(redactFileOperationError("rewind source spool", err))
	}
	return temporary, cleanup, nil
}

func redactFileOperationError(operation string, err error) error {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Errorf("%s: %w", operation, pathErr.Err)
	}
	return fmt.Errorf("%s: %w", operation, err)
}

func copyExactSource(ctx context.Context, destination io.Writer, source io.Reader, expectedSize int64) (int64, error) {
	if expectedSize < 0 {
		return 0, fmt.Errorf("document source size cannot be negative")
	}
	limited := &io.LimitedReader{R: &sourceContextReader{ctx: ctx, reader: source}, N: expectedSize + 1}
	buffer := make([]byte, sourceCopyBufferBytes)
	written, err := io.CopyBuffer(destination, limited, buffer)
	if err != nil {
		return written, fmt.Errorf("stream document source: %w", err)
	}
	if written > expectedSize {
		return written, fmt.Errorf("%w: source exceeds declared size %d", ErrSourceIntegrity, expectedSize)
	}
	if err := ctx.Err(); err != nil {
		return written, err
	}
	return written, nil
}

type sourceContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *sourceContextReader) Read(value []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(value)
}
