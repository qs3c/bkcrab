package parseeval

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/rag/document"
)

var (
	ErrRendererUnavailable   = errors.New("parser evaluation: renderer unavailable")
	ErrInvalidRendererBundle = errors.New("parser evaluation: invalid renderer bundle")
	ErrRendererLimit         = errors.New("parser evaluation: renderer limit exceeded")
)

type RendererClient interface {
	Render(context.Context, document.Source) (*RenderedTruth, error)
	HealthSnapshot() RendererHealthSnapshot
	StartHealthProbe(context.Context)
}

type RendererHealthLimits struct {
	MaxInputBytes  int64 `json:"maxInputBytes"`
	MaxPages       int   `json:"maxPages"`
	RenderDPI      int   `json:"renderDPI"`
	MaxPagePixels  int64 `json:"maxPagePixels"`
	MaxPageBytes   int64 `json:"maxPageBytes"`
	MaxBundleBytes int64 `json:"maxBundleBytes"`
}

func (limits RendererHealthLimits) Validate() error {
	if limits.MaxInputBytes <= 0 || limits.MaxPages <= 0 || limits.MaxPages > 100 || limits.RenderDPI < 36 || limits.RenderDPI > 300 ||
		limits.MaxPagePixels <= 0 || limits.MaxPageBytes <= 0 || limits.MaxBundleBytes < limits.MaxPageBytes {
		return errors.New("invalid renderer limits")
	}
	return nil
}

type RendererHealthSnapshot struct {
	Descriptor RendererDescriptor
	Limits     RendererHealthLimits
	Formats    []Format
	Healthy    bool
	Reason     string
	CheckedAt  time.Time
	ExpiresAt  time.Time
}

type RendererClientConfig struct {
	Endpoint            string
	RequestTimeout      time.Duration
	HealthTTL           time.Duration
	HealthProbeInterval time.Duration
	ExpectedMaxPages    int
	ExpectedRenderDPI   int
	MaxInputBytes       int64
	MaxManifestBytes    int64
	MaxArchiveBytes     int64
	MaxPageBytes        int64
	MaxPagePixels       int64
	TempDir             string
}

func DefaultRendererClientConfig(endpoint string) RendererClientConfig {
	return RendererClientConfig{
		Endpoint: endpoint, RequestTimeout: 10 * time.Minute, HealthTTL: 30 * time.Second,
		HealthProbeInterval: 15 * time.Second, ExpectedMaxPages: 6, ExpectedRenderDPI: 100,
		MaxInputBytes: 50 << 20, MaxManifestBytes: 1 << 20, MaxArchiveBytes: 128 << 20,
		MaxPageBytes: 20 << 20, MaxPagePixels: 40_000_000,
	}
}

type HTTPRendererClient struct {
	endpoint   *url.URL
	httpClient *http.Client
	config     RendererClientConfig
	now        func() time.Time

	healthMu sync.RWMutex
	health   RendererHealthSnapshot
	probeMu  sync.Mutex
	start    sync.Once
}

func NewHTTPRendererClient(cfg RendererClientConfig, httpClient *http.Client) (*HTTPRendererClient, error) {
	endpoint, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil ||
		(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, errors.New("renderer endpoint must be an absolute credential-free HTTP URL without a path")
	}
	if cfg.RequestTimeout <= 0 || cfg.HealthTTL <= 0 || cfg.HealthProbeInterval <= 0 || cfg.ExpectedMaxPages <= 0 ||
		cfg.ExpectedRenderDPI <= 0 || cfg.MaxInputBytes <= 0 || cfg.MaxManifestBytes <= 0 || cfg.MaxArchiveBytes <= 0 ||
		cfg.MaxPageBytes <= 0 || cfg.MaxPagePixels <= 0 {
		return nil, errors.New("invalid renderer client configuration")
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	return &HTTPRendererClient{endpoint: endpoint, httpClient: httpClient, config: cfg, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (c *HTTPRendererClient) endpointURL(route string, query url.Values) string {
	resolved := *c.endpoint
	resolved.Path = path.Join("/", route)
	resolved.RawQuery = query.Encode()
	return resolved.String()
}

type rendererHealthResponse struct {
	Descriptor RendererDescriptor   `json:"descriptor"`
	Formats    []Format             `json:"formats"`
	Limits     RendererHealthLimits `json:"limits"`
}

func (response rendererHealthResponse) Validate() error {
	if err := response.Descriptor.Validate(); err != nil {
		return err
	}
	if err := response.Limits.Validate(); err != nil {
		return err
	}
	wanted := []Format{FormatDOCX, FormatPPTX, FormatXLSX}
	if len(response.Formats) != len(wanted) {
		return errors.New("renderer must advertise exactly docx, pptx, and xlsx")
	}
	for index := range wanted {
		if response.Formats[index] != wanted[index] {
			return errors.New("renderer formats are not canonical")
		}
	}
	return nil
}

func (c *HTTPRendererClient) HealthSnapshot() RendererHealthSnapshot {
	if c == nil {
		return RendererHealthSnapshot{}
	}
	c.healthMu.RLock()
	snapshot := c.health
	snapshot.Formats = append([]Format(nil), snapshot.Formats...)
	c.healthMu.RUnlock()
	return snapshot
}

func (c *HTTPRendererClient) ProbeHealth(ctx context.Context) (RendererHealthSnapshot, error) {
	if c == nil {
		return RendererHealthSnapshot{}, ErrRendererUnavailable
	}
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	checkedAt := c.now().UTC()
	probeCtx, cancel := context.WithTimeout(ctx, minDuration(c.config.RequestTimeout, 5*time.Second))
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, c.endpointURL("healthz", nil), nil)
	if err != nil {
		return c.storeHealthFailure(checkedAt, "invalid_request"), err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		if probeCtx.Err() != nil {
			err = probeCtx.Err()
		}
		return c.storeHealthFailure(checkedAt, "unreachable"), err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return c.storeHealthFailure(checkedAt, fmt.Sprintf("http_%d", response.StatusCode)), ErrRendererUnavailable
	}
	mediaType, parameters, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaErr != nil || mediaType != "application/json" || len(parameters) != 0 {
		return c.storeHealthFailure(checkedAt, "invalid_protocol"), ErrInvalidRendererBundle
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 {
		return c.storeHealthFailure(checkedAt, "invalid_protocol"), ErrInvalidRendererBundle
	}
	protocol, err := DecodeClosedJSON[rendererHealthResponse](body, 1<<20, func(value *rendererHealthResponse) error { return value.Validate() })
	if err != nil || protocol.Limits.MaxPages != c.config.ExpectedMaxPages || protocol.Limits.RenderDPI != c.config.ExpectedRenderDPI {
		return c.storeHealthFailure(checkedAt, "protocol_mismatch"), fmt.Errorf("%w: renderer health mismatch", ErrRendererUnavailable)
	}
	snapshot := RendererHealthSnapshot{
		Descriptor: protocol.Descriptor, Limits: protocol.Limits, Formats: append([]Format(nil), protocol.Formats...),
		Healthy: true, CheckedAt: checkedAt, ExpiresAt: checkedAt.Add(c.config.HealthTTL),
	}
	c.healthMu.Lock()
	c.health = snapshot
	c.healthMu.Unlock()
	return snapshot, nil
}

func (c *HTTPRendererClient) storeHealthFailure(checkedAt time.Time, reason string) RendererHealthSnapshot {
	snapshot := RendererHealthSnapshot{Healthy: false, Reason: reason, CheckedAt: checkedAt, ExpiresAt: checkedAt.Add(c.config.HealthTTL)}
	c.healthMu.Lock()
	c.health = snapshot
	c.healthMu.Unlock()
	return snapshot
}

func (c *HTTPRendererClient) StartHealthProbe(ctx context.Context) {
	if c == nil {
		return
	}
	c.start.Do(func() {
		go func() {
			_, _ = c.ProbeHealth(ctx)
			ticker := time.NewTicker(c.config.HealthProbeInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, _ = c.ProbeHealth(ctx)
				}
			}
		}()
	})
}

func (c *HTTPRendererClient) currentHealth() (RendererHealthSnapshot, bool) {
	snapshot := c.HealthSnapshot()
	return snapshot, snapshot.Healthy && c.now().Before(snapshot.ExpiresAt)
}

func (c *HTTPRendererClient) Render(ctx context.Context, source document.Source) (*RenderedTruth, error) {
	health, fresh := c.currentHealth()
	if !fresh {
		return nil, ErrRendererUnavailable
	}
	format, err := validateRendererSource(source, minInt64(c.config.MaxInputBytes, health.Limits.MaxInputBytes))
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.config.RequestTimeout)
	defer cancel()
	pipeReader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- writeRendererMultipart(requestCtx, pipeWriter, multipartWriter, source, format)
	}()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.endpointURL("v1/render", url.Values{"format": {string(format)}}), pipeReader)
	if err != nil {
		_ = pipeReader.CloseWithError(err)
		<-writerDone
		return nil, err
	}
	request.Header.Set("Content-Type", multipartWriter.FormDataContentType())
	request.Header.Set("Accept", "application/x-tar")
	response, err := c.httpClient.Do(request)
	if err != nil {
		_ = pipeReader.CloseWithError(err)
		<-writerDone
		if requestCtx.Err() != nil {
			return nil, requestCtx.Err()
		}
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = pipeReader.CloseWithError(ErrRendererUnavailable)
		<-writerDone
		return nil, &RendererHTTPError{StatusCode: response.StatusCode}
	}
	if err := <-writerDone; err != nil {
		return nil, err
	}
	mediaType, parameters, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-tar" || len(parameters) != 0 {
		return nil, fmt.Errorf("%w: invalid content type", ErrInvalidRendererBundle)
	}
	if response.ContentLength > 0 && response.ContentLength > minInt64(c.config.MaxArchiveBytes, health.Limits.MaxBundleBytes) {
		return nil, ErrRendererLimit
	}
	return c.decodeRenderBundle(requestCtx, response.Body, source, health)
}

type RendererHTTPError struct{ StatusCode int }

func (e *RendererHTTPError) Error() string {
	return fmt.Sprintf("renderer returned HTTP %d", e.StatusCode)
}

func validateRendererSource(source document.Source, maxBytes int64) (Format, error) {
	if err := source.Validate(); err != nil {
		return "", err
	}
	format := Format(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(source.Format)), "."))
	if !format.Valid() || source.Size <= 0 || source.Size > maxBytes {
		return "", ErrRendererLimit
	}
	if strings.ToLower(filepath.Ext(source.FileName)) != "."+string(format) || filepath.Base(source.FileName) != source.FileName || strings.ContainsAny(source.FileName, "\\\r\n\x00") {
		return "", errors.New("invalid renderer source file name")
	}
	return format, nil
}

func rendererMIME(format Format) string {
	switch format {
	case FormatDOCX:
		return MediaTypeDOCX
	case FormatPPTX:
		return MediaTypePPTX
	case FormatXLSX:
		return MediaTypeXLSX
	default:
		return "application/octet-stream"
	}
}

func writeRendererMultipart(ctx context.Context, pipe *io.PipeWriter, writer *multipart.Writer, source document.Source, format Format) (resultErr error) {
	defer func() { _ = pipe.CloseWithError(resultErr) }()
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, strings.ReplaceAll(source.FileName, `"`, `'`)))
	header.Set("Content-Type", rendererMIME(format))
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	reader, err := source.Open(ctx)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(part, hasher), io.LimitReader(&rendererContextReader{ctx: ctx, reader: reader}, source.Size+1))
	closeErr := reader.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written != source.Size || hex.EncodeToString(hasher.Sum(nil)) != source.SHA256 {
		return errors.New("renderer source changed while streaming")
	}
	return writer.Close()
}

type rendererContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *rendererContextReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

type rendererSourceDescriptor struct {
	Format   Format `json:"format"`
	ByteSize int64  `json:"byteSize"`
	SHA256   string `json:"sha256"`
}

type rendererPageDescriptor struct {
	Page      int    `json:"page"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
	SHA256    string `json:"sha256"`
	ByteSize  int64  `json:"byteSize"`
}

type rendererManifest struct {
	ProtocolVersion  string                   `json:"protocolVersion"`
	Descriptor       RendererDescriptor       `json:"descriptor"`
	Source           rendererSourceDescriptor `json:"source"`
	TotalPages       int                      `json:"totalPages"`
	CoveredPages     int                      `json:"coveredPages"`
	RenderDurationMS int64                    `json:"renderDurationMs"`
	Pages            []rendererPageDescriptor `json:"pages"`
}

func (manifest rendererManifest) validate(source document.Source, health RendererHealthSnapshot) error {
	if manifest.ProtocolVersion != "parser-eval-renderer/v1" || manifest.Descriptor != health.Descriptor || manifest.RenderDurationMS < 0 ||
		manifest.Source.Format != Format(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(source.Format)), ".")) || manifest.Source.ByteSize != source.Size || manifest.Source.SHA256 != source.SHA256 ||
		manifest.TotalPages <= 0 || manifest.CoveredPages <= 0 || manifest.CoveredPages != len(manifest.Pages) ||
		manifest.CoveredPages > manifest.TotalPages || manifest.CoveredPages > health.Limits.MaxPages {
		return errors.New("renderer manifest identity mismatch")
	}
	for index, page := range manifest.Pages {
		expectedPath := fmt.Sprintf("pages/page-%04d.png", index+1)
		if page.Page != index+1 || page.Path != expectedPath || page.MediaType != "image/png" || page.Width <= 0 || page.Height <= 0 ||
			page.ByteSize <= 0 || !canonicalSHA256(page.SHA256) || int64(page.Width)*int64(page.Height) > health.Limits.MaxPagePixels {
			return errors.New("invalid renderer page descriptor")
		}
	}
	return nil
}

type RenderedPage struct {
	Page      int
	Width     int
	Height    int
	SHA256    string
	ByteSize  int64
	entryPath string
}

type RenderedTruth struct {
	Descriptor       RendererDescriptor
	TotalPages       int
	CoveredPages     int
	RenderDurationMS int64
	Pages            []RenderedPage

	root    string
	entries map[int]string
	mu      sync.RWMutex
	closed  bool
	once    sync.Once
	err     error
}

func (truth *RenderedTruth) OpenPage(ctx context.Context, pageNumber int) (io.ReadCloser, error) {
	if truth == nil {
		return nil, os.ErrNotExist
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	truth.mu.RLock()
	defer truth.mu.RUnlock()
	if truth.closed {
		return nil, os.ErrClosed
	}
	local, exists := truth.entries[pageNumber]
	if !exists {
		return nil, os.ErrNotExist
	}
	return os.Open(local)
}

func (truth *RenderedTruth) Close() error {
	if truth == nil {
		return nil
	}
	truth.once.Do(func() {
		truth.mu.Lock()
		truth.closed = true
		truth.err = os.RemoveAll(truth.root)
		truth.mu.Unlock()
	})
	return truth.err
}

func (c *HTTPRendererClient) decodeRenderBundle(ctx context.Context, source io.Reader, expected document.Source, health RendererHealthSnapshot) (_ *RenderedTruth, resultErr error) {
	archiveLimit := minInt64(c.config.MaxArchiveBytes, health.Limits.MaxBundleBytes)
	reader := tar.NewReader(&rendererHardLimitReader{reader: &rendererContextReader{ctx: ctx, reader: source}, remaining: archiveLimit})
	manifestHeader, err := reader.Next()
	if errors.Is(err, ErrRendererLimit) || err == nil && manifestHeader.Size > c.config.MaxManifestBytes {
		return nil, ErrRendererLimit
	}
	if err != nil || !validRendererTarHeader(manifestHeader) || manifestHeader.Name != "manifest.json" || manifestHeader.Size < 0 {
		return nil, fmt.Errorf("%w: manifest must be the first bounded regular entry", ErrInvalidRendererBundle)
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(reader, c.config.MaxManifestBytes+1))
	if err != nil || int64(len(manifestBytes)) != manifestHeader.Size {
		return nil, fmt.Errorf("%w: invalid manifest bytes", ErrInvalidRendererBundle)
	}
	manifest, err := DecodeClosedJSON[rendererManifest](manifestBytes, int(c.config.MaxManifestBytes), func(value *rendererManifest) error {
		return value.validate(expected, health)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRendererBundle, err)
	}
	root, err := os.MkdirTemp(c.config.TempDir, "bkcrab-parser-render-*")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	truth := &RenderedTruth{
		Descriptor: manifest.Descriptor, TotalPages: manifest.TotalPages, CoveredPages: manifest.CoveredPages,
		RenderDurationMS: manifest.RenderDurationMS, Pages: make([]RenderedPage, 0, len(manifest.Pages)),
		root: root, entries: make(map[int]string, len(manifest.Pages)),
	}
	defer func() {
		if resultErr != nil {
			_ = truth.Close()
		}
	}()
	pageLimit := minInt64(c.config.MaxPageBytes, health.Limits.MaxPageBytes)
	for _, descriptor := range manifest.Pages {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, ErrRendererLimit) || nextErr == nil && header.Size > pageLimit {
			return nil, ErrRendererLimit
		}
		if nextErr != nil || !validRendererTarHeader(header) || header.Name != descriptor.Path || header.Size != descriptor.ByteSize {
			return nil, fmt.Errorf("%w: page entry mismatch", ErrInvalidRendererBundle)
		}
		localPath := filepath.Join(root, filepath.FromSlash(descriptor.Path))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o700); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(localPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		hasher := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(file, hasher), reader)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil || written != descriptor.ByteSize || hex.EncodeToString(hasher.Sum(nil)) != descriptor.SHA256 {
			return nil, fmt.Errorf("%w: page hash or size mismatch", ErrInvalidRendererBundle)
		}
		imageFile, err := os.Open(localPath)
		if err != nil {
			return nil, err
		}
		imageConfig, imageFormat, decodeErr := image.DecodeConfig(imageFile)
		_ = imageFile.Close()
		if decodeErr != nil || imageFormat != "png" || imageConfig.Width != descriptor.Width || imageConfig.Height != descriptor.Height ||
			int64(imageConfig.Width)*int64(imageConfig.Height) > minInt64(c.config.MaxPagePixels, health.Limits.MaxPagePixels) {
			return nil, fmt.Errorf("%w: invalid PNG dimensions", ErrInvalidRendererBundle)
		}
		truth.entries[descriptor.Page] = localPath
		truth.Pages = append(truth.Pages, RenderedPage{
			Page: descriptor.Page, Width: descriptor.Width, Height: descriptor.Height,
			SHA256: descriptor.SHA256, ByteSize: descriptor.ByteSize, entryPath: descriptor.Path,
		})
	}
	if extra, nextErr := reader.Next(); nextErr == nil {
		return nil, fmt.Errorf("%w: undeclared entry %q", ErrInvalidRendererBundle, extra.Name)
	} else if !errors.Is(nextErr, io.EOF) {
		if errors.Is(nextErr, ErrRendererLimit) {
			return nil, nextErr
		}
		return nil, fmt.Errorf("%w: finish tar: %v", ErrInvalidRendererBundle, nextErr)
	}
	return truth, nil
}

func validRendererTarHeader(header *tar.Header) bool {
	return header != nil && (header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA) && header.Linkname == "" &&
		len(header.PAXRecords) == 0 && len(header.Xattrs) == 0 && header.Format != tar.FormatPAX && header.Format != tar.FormatGNU
}

type rendererHardLimitReader struct {
	reader    io.Reader
	remaining int64
}

func (reader *rendererHardLimitReader) Read(buffer []byte) (int, error) {
	if reader.remaining <= 0 {
		var probe [1]byte
		count, err := reader.reader.Read(probe[:])
		if count > 0 {
			return 0, ErrRendererLimit
		}
		return 0, err
	}
	if int64(len(buffer)) > reader.remaining {
		buffer = buffer[:reader.remaining]
	}
	count, err := reader.reader.Read(buffer)
	reader.remaining -= int64(count)
	return count, err
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

var _ RendererClient = (*HTTPRendererClient)(nil)
