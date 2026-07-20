package sidecar

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"
)

const defaultMaxManifestBytes int64 = 1 << 20

// BundleHandle owns the bounded temporary directory created while decoding a
// sidecar tar stream. Close is idempotent; callers transferring entry access
// to ParsedDocument should transfer Close as the document cleanup function.
type BundleHandle struct {
	Manifest Manifest

	root     string
	entries  map[string]string
	mu       sync.RWMutex
	closed   bool
	once     sync.Once
	closeErr error
}

func (h *BundleHandle) OpenEntry(ctx context.Context, entryPath string) (io.ReadCloser, error) {
	if h == nil {
		return nil, errors.New("nil sidecar bundle handle")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validBundlePath(entryPath) {
		return nil, invalidBundle("unsafe bundle entry path %q", entryPath)
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return nil, os.ErrClosed
	}
	localPath, ok := h.entries[entryPath]
	if !ok {
		return nil, os.ErrNotExist
	}
	file, err := os.Open(localPath)
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (h *BundleHandle) EntryPath(entryPath string) (string, error) {
	if h == nil || !validBundlePath(entryPath) {
		return "", invalidBundle("unsafe or nil bundle entry")
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return "", os.ErrClosed
	}
	localPath, ok := h.entries[entryPath]
	if !ok {
		return "", os.ErrNotExist
	}
	return localPath, nil
}

func (h *BundleHandle) PagePrimitive(ctx context.Context, pageNumber int) (PagePrimitive, error) {
	if h == nil {
		return PagePrimitive{}, errors.New("nil sidecar bundle handle")
	}
	entryPath := ""
	for _, pageDescriptor := range h.Manifest.Pages {
		if pageDescriptor.Page == pageNumber {
			entryPath = pageDescriptor.PrimitiveEntry
			break
		}
	}
	if entryPath == "" {
		return PagePrimitive{}, os.ErrNotExist
	}
	entry, err := h.OpenEntry(ctx, entryPath)
	if err != nil {
		return PagePrimitive{}, err
	}
	defer entry.Close()
	primitive, err := DecodePagePrimitive(entry)
	if err != nil {
		return PagePrimitive{}, err
	}
	if primitive.Page != pageNumber {
		return PagePrimitive{}, invalidBundle("primitive page=%d, expected %d", primitive.Page, pageNumber)
	}
	return primitive, nil
}

func (h *BundleHandle) Close() error {
	if h == nil {
		return nil
	}
	h.once.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.closeErr = os.RemoveAll(h.root)
		h.mu.Unlock()
	})
	return h.closeErr
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(buffer)
}

func rejectTarHeader(header *tar.Header) error {
	if header == nil {
		return invalidBundle("nil tar header")
	}
	if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
		return invalidBundle("tar entry %q has forbidden type %d", header.Name, header.Typeflag)
	}
	if header.Linkname != "" || len(header.PAXRecords) != 0 || len(header.Xattrs) != 0 {
		return invalidBundle("tar entry %q uses link/PAX/xattr metadata", header.Name)
	}
	if header.Format == tar.FormatPAX || header.Format == tar.FormatGNU {
		return invalidBundle("tar entry %q uses a non-USTAR extension", header.Name)
	}
	return nil
}

func bundleStreamError(operation string, err error) error {
	if errors.Is(err, ErrBundleLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return invalidBundle("%s: %v", operation, err)
}

func readManifest(reader *tar.Reader, limits DecodeLimits) ([]byte, error) {
	header, err := reader.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, invalidBundle("empty tar response")
		}
		return nil, bundleStreamError("read first tar header", err)
	}
	if err := rejectTarHeader(header); err != nil {
		return nil, err
	}
	if header.Name != ManifestEntryName {
		return nil, invalidBundle("manifest.json must be the first tar entry")
	}
	maxManifest := limits.MaxManifestBytes
	if maxManifest <= 0 {
		maxManifest = defaultMaxManifestBytes
	}
	if header.Size < 0 || header.Size > maxManifest {
		return nil, limitExceeded("manifest size %d exceeds %d", header.Size, maxManifest)
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxManifest+1))
	if err != nil {
		return nil, bundleStreamError("read manifest", err)
	}
	if int64(len(data)) != header.Size {
		return nil, invalidBundle("manifest size mismatch")
	}
	return data, nil
}

// DecodeBundle strictly validates and incrementally extracts an untrusted tar
// stream. Manifest structure is checked before any payload path is created.
func DecodeBundle(ctx context.Context, source io.Reader, options DecodeOptions) (_ *BundleHandle, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	archiveSource := io.Reader(&contextReader{ctx: ctx, r: source})
	if options.Limits.MaxArchiveBytes > 0 {
		archiveSource = &hardLimitReader{reader: archiveSource, remaining: options.Limits.MaxArchiveBytes}
	}
	reader := tar.NewReader(archiveSource)
	manifestJSON, err := readManifest(reader, options.Limits)
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := decodeStrict(manifestJSON, &manifest); err != nil {
		return nil, invalidBundle("decode manifest: %v", err)
	}
	if err := ValidateManifest(&manifest, options); err != nil {
		return nil, err
	}
	totalBytes := int64(len(manifestJSON))
	if options.Limits.MaxTotalBytes > 0 && totalBytes > options.Limits.MaxTotalBytes {
		return nil, limitExceeded("manifest exceeds total extraction quota")
	}
	root, err := os.MkdirTemp(options.TempDir, "bkcrab-rag-parser-*")
	if err != nil {
		return nil, fmt.Errorf("create sidecar bundle temp dir: %w", err)
	}
	if chmodErr := os.Chmod(root, 0o700); chmodErr != nil {
		_ = os.RemoveAll(root)
		return nil, fmt.Errorf("secure sidecar bundle temp dir: %w", chmodErr)
	}
	handle := &BundleHandle{Manifest: manifest, root: root, entries: make(map[string]string, len(manifest.Entries))}
	defer func() {
		if err != nil {
			_ = handle.Close()
		}
	}()

	for _, descriptor := range manifest.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, nextErr := reader.Next()
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				return nil, invalidBundle("missing tar entry %q", descriptor.Path)
			}
			return nil, bundleStreamError("read tar header", nextErr)
		}
		if err := rejectTarHeader(header); err != nil {
			return nil, err
		}
		if header.Name != descriptor.Path {
			return nil, invalidBundle("tar entry %q, expected %q", header.Name, descriptor.Path)
		}
		if !validBundlePath(header.Name) || header.Size != descriptor.ByteSize {
			return nil, invalidBundle("tar entry %q size/path mismatch", header.Name)
		}
		if options.Limits.MaxEntryBytes > 0 && header.Size > options.Limits.MaxEntryBytes {
			return nil, limitExceeded("entry %q exceeds single-entry quota", header.Name)
		}
		if options.Limits.MaxTotalBytes > 0 && (header.Size > options.Limits.MaxTotalBytes-totalBytes) {
			return nil, limitExceeded("entry %q exceeds total extraction quota", header.Name)
		}
		totalBytes += header.Size

		localPath := filepath.Join(root, filepath.FromSlash(descriptor.Path))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
			return nil, fmt.Errorf("create bundle entry directory: %w", err)
		}
		file, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create bundle entry: %w", err)
		}
		hasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(file, hasher), &contextReader{ctx: ctx, r: reader})
		closeErr := file.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if written != descriptor.ByteSize || hex.EncodeToString(hasher.Sum(nil)) != descriptor.SHA256 {
			return nil, invalidBundle("entry %q hash/size mismatch", descriptor.Path)
		}
		if err := verifyEntryFile(localPath, descriptor); err != nil {
			return nil, err
		}
		handle.entries[descriptor.Path] = localPath
	}
	if header, nextErr := reader.Next(); nextErr == nil {
		return nil, invalidBundle("undeclared tar entry %q", header.Name)
	} else if !errors.Is(nextErr, io.EOF) {
		return nil, bundleStreamError("finish tar stream", nextErr)
	}
	for _, pageDescriptor := range manifest.Pages {
		if pageDescriptor.PrimitiveEntry == "" {
			continue
		}
		primitiveFile, openErr := os.Open(handle.entries[pageDescriptor.PrimitiveEntry])
		if openErr != nil {
			return nil, openErr
		}
		primitive, primitiveErr := DecodePagePrimitive(primitiveFile)
		closeErr := primitiveFile.Close()
		if primitiveErr != nil {
			return nil, primitiveErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if primitive.Page != pageDescriptor.Page {
			return nil, invalidBundle("primitive page=%d, expected %d", primitive.Page, pageDescriptor.Page)
		}
	}
	return handle, nil
}

func verifyEntryFile(localPath string, descriptor EntryDescriptor) error {
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()
	if descriptor.MIMEType == MIMETypeMarkdown {
		reader := bufio.NewReader(file)
		for {
			runeValue, size, readErr := reader.ReadRune()
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				return readErr
			}
			if runeValue == utf8.RuneError && size == 1 {
				return invalidBundle("markdown entry %q is not UTF-8", descriptor.Path)
			}
		}
	}
	if descriptor.MIMEType == MIMETypeJSON {
		decoder := json.NewDecoder(&strictUTF8Reader{reader: file})
		var value any
		if err := decoder.Decode(&value); err != nil {
			return invalidBundle("JSON entry %q is invalid", descriptor.Path)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return invalidBundle("JSON entry %q contains trailing data", descriptor.Path)
		}
		return nil
	}
	prefix := make([]byte, 512)
	read, readErr := file.Read(prefix)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	prefix = prefix[:read]
	detected := http.DetectContentType(prefix)
	if detected != descriptor.MIMEType {
		return invalidBundle("entry %q MIME is %q, declared %q", descriptor.Path, detected, descriptor.MIMEType)
	}
	return nil
}

type hardLimitReader struct {
	reader    io.Reader
	remaining int64
}

func (r *hardLimitReader) Read(buffer []byte) (int, error) {
	if r.remaining <= 0 {
		var probe [1]byte
		read, err := r.reader.Read(probe[:])
		if read == 0 && errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		return 0, limitExceeded("tar stream exceeds archive byte limit")
	}
	if int64(len(buffer)) > r.remaining+1 {
		buffer = buffer[:r.remaining+1]
	}
	read, err := r.reader.Read(buffer)
	r.remaining -= int64(read)
	if r.remaining < 0 {
		return read, limitExceeded("tar stream exceeds archive byte limit")
	}
	return read, err
}
