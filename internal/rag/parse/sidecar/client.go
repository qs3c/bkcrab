package sidecar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/rag/document"
	"github.com/qs3c/bkcrab/internal/rag/telemetry"
)

const (
	ExpectedMarkItDownVersion = "0.1.6"
	ExpectedOfficeWrapper     = "office-wrapper-v2"
	expectedPDFEngine         = "pypdfium2"
	expectedPDFEngineVersion  = "5.12.1"

	// These build-time gates are promoted only together with the Task 16
	// checked-in three-format positioning goldens. Health compatibility alone
	// is intentionally insufficient.
	officeDOCXGolden = true
	officePPTXGolden = true
	officeXLSXGolden = true
)

type ClientLimits struct {
	MaxInputBytes     int64
	MaxOutputBytes    int64
	MaxExtractedBytes int64
	MaxEntryBytes     int64
	MaxAssetBytes     int64
	MaxRenderBytes    int64
	MaxEntries        int
	MaxPages          int
	MaxAssets         int
	MaxImagePixels    int64
	MaxManifestBytes  int64
}

type ClientConfig struct {
	Endpoint            string
	Timeout             time.Duration
	HealthTTL           time.Duration
	HealthProbeInterval time.Duration
	Limits              ClientLimits
	TempDir             string
	HTTPClient          *http.Client
	Now                 func() time.Time
	PDFLicenseApproved  bool
	Recorder            telemetry.Recorder
}

type cachedHealth struct {
	protocol Health
	snapshot config.RAGParserHealthSnapshot
}

type Client struct {
	endpoint            *url.URL
	httpClient          *http.Client
	timeout             time.Duration
	healthTTL           time.Duration
	healthProbeInterval time.Duration
	limits              ClientLimits
	tempDir             string
	now                 func() time.Time
	pdfLicenseApproved  bool
	recorder            telemetry.Recorder

	probeMu   sync.Mutex
	healthMu  sync.RWMutex
	health    cachedHealth
	startOnce sync.Once
}

func defaultClientLimits(limits ClientLimits) ClientLimits {
	if limits.MaxInputBytes <= 0 {
		limits.MaxInputBytes = 50 << 20
	}
	if limits.MaxOutputBytes <= 0 {
		limits.MaxOutputBytes = 200 << 20
	}
	if limits.MaxExtractedBytes <= 0 {
		limits.MaxExtractedBytes = limits.MaxOutputBytes
	}
	if limits.MaxEntryBytes <= 0 {
		limits.MaxEntryBytes = 50 << 20
	}
	if limits.MaxAssetBytes <= 0 {
		limits.MaxAssetBytes = 20 << 20
	}
	if limits.MaxRenderBytes <= 0 {
		limits.MaxRenderBytes = 8 << 20
	}
	if limits.MaxEntries <= 0 {
		limits.MaxEntries = 2048
	}
	if limits.MaxPages <= 0 {
		limits.MaxPages = 300
	}
	if limits.MaxAssets <= 0 {
		limits.MaxAssets = 500
	}
	if limits.MaxImagePixels <= 0 {
		limits.MaxImagePixels = 40_000_000
	}
	if limits.MaxManifestBytes <= 0 {
		limits.MaxManifestBytes = defaultMaxManifestBytes
	}
	return limits
}

func NewClient(clientConfig ClientConfig) (*Client, error) {
	endpoint, err := url.Parse(strings.TrimSpace(clientConfig.Endpoint))
	if err != nil || endpoint == nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("invalid rag parser endpoint %q", clientConfig.Endpoint)
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/")
	if clientConfig.Timeout <= 0 {
		clientConfig.Timeout = 10 * time.Second
	}
	if clientConfig.HealthTTL <= 0 {
		clientConfig.HealthTTL = 30 * time.Second
	}
	if clientConfig.HealthProbeInterval <= 0 {
		clientConfig.HealthProbeInterval = clientConfig.HealthTTL / 2
	}
	if clientConfig.HealthProbeInterval <= 0 {
		clientConfig.HealthProbeInterval = time.Second
	}
	now := clientConfig.Now
	if now == nil {
		now = time.Now
	}
	if clientConfig.Recorder == nil {
		clientConfig.Recorder = telemetry.NewSlogRecorder(nil)
	}
	baseHTTPClient := http.Client{}
	if clientConfig.HTTPClient != nil {
		baseHTTPClient = *clientConfig.HTTPClient
	}
	baseHTTPClient.Timeout = clientConfig.Timeout
	baseHTTPClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		endpoint: endpoint, httpClient: &baseHTTPClient, timeout: clientConfig.Timeout,
		healthTTL: clientConfig.HealthTTL, healthProbeInterval: clientConfig.HealthProbeInterval,
		limits: defaultClientLimits(clientConfig.Limits), tempDir: clientConfig.TempDir,
		now: now, pdfLicenseApproved: clientConfig.PDFLicenseApproved,
		recorder: clientConfig.Recorder,
	}, nil
}

// SetRecorder replaces the privacy-safe sidecar observability sink. Source
// bytes, bundle contents, temporary paths, endpoints and credentials are not
// representable by the closed telemetry schema.
func (c *Client) SetRecorder(recorder telemetry.Recorder) {
	if c == nil {
		return
	}
	if recorder == nil {
		recorder = telemetry.NopRecorder()
	}
	c.recorder = recorder
}

func (c *Client) requestURL(endpointPath string, query url.Values) string {
	resolved := *c.endpoint
	resolved.Path = path.Join(c.endpoint.Path, endpointPath)
	if !strings.HasPrefix(resolved.Path, "/") {
		resolved.Path = "/" + resolved.Path
	}
	resolved.RawQuery = query.Encode()
	return resolved.String()
}

func (c *Client) HealthSnapshot() config.RAGParserHealthSnapshot {
	if c == nil {
		return config.RAGParserHealthSnapshot{}
	}
	c.healthMu.RLock()
	snapshot := c.health.snapshot
	snapshot.Office.Formats = append([]string(nil), snapshot.Office.Formats...)
	c.healthMu.RUnlock()
	return snapshot
}

func (c *Client) storeProbeFailure(now time.Time, err error) config.RAGParserHealthSnapshot {
	reason := "parser_health_unavailable"
	var statusError *HTTPError
	if errors.As(err, &statusError) {
		reason = "parser_health_http_" + strconv.Itoa(statusError.StatusCode)
	} else if errors.Is(err, ErrInvalidBundle) {
		reason = "parser_health_protocol_invalid"
	} else if errors.Is(err, context.DeadlineExceeded) {
		reason = "parser_health_timeout"
	}
	snapshot := config.RAGParserHealthSnapshot{
		Healthy: false, Reason: reason, CheckedAt: now, ExpiresAt: now.Add(c.healthTTL),
	}
	c.healthMu.Lock()
	c.health = cachedHealth{snapshot: snapshot}
	c.healthMu.Unlock()
	return snapshot
}

func (c *Client) ProbeHealth(ctx context.Context) (config.RAGParserHealthSnapshot, error) {
	if c == nil {
		return config.RAGParserHealthSnapshot{}, unavailable("health", "client_not_configured")
	}
	c.probeMu.Lock()
	defer c.probeMu.Unlock()
	now := c.now().UTC()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.requestURL("/healthz", nil), nil)
	if err != nil {
		return c.storeProbeFailure(now, err), err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return c.storeProbeFailure(now, err), err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		err = &HTTPError{Operation: "health", StatusCode: response.StatusCode}
		return c.storeProbeFailure(now, err), err
	}
	const maxHealthBytes = int64(1 << 20)
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHealthBytes+1))
	if err != nil {
		return c.storeProbeFailure(now, err), err
	}
	if int64(len(body)) > maxHealthBytes {
		err = limitExceeded("health response exceeds 1 MiB")
		return c.storeProbeFailure(now, err), err
	}
	var health Health
	if err := decodeStrict(body, &health); err != nil {
		err = invalidBundle("decode health: %v", err)
		return c.storeProbeFailure(now, err), err
	}
	if err := validateHealth(&health); err != nil {
		return c.storeProbeFailure(now, err), err
	}
	officeCompatible := health.Capabilities.Office.Enabled &&
		health.Capabilities.Office.MarkItDownVersion == ExpectedMarkItDownVersion &&
		health.Capabilities.Office.WrapperVersion == ExpectedOfficeWrapper
	pdfCompatible := health.Capabilities.PDF.Enabled &&
		health.Capabilities.PDF.Engine == expectedPDFEngine &&
		health.Capabilities.PDF.EngineVersion == expectedPDFEngineVersion
	snapshot := config.RAGParserHealthSnapshot{
		ProtocolVersion: health.ProtocolVersion,
		Healthy:         true,
		CheckedAt:       now,
		ExpiresAt:       now.Add(c.healthTTL),
		MaxInputBytes:   health.Limits.MaxInputBytes,
		Office: config.RAGParserOfficeSnapshot{
			// Keep capability publication and ConvertOffice on the same exact
			// implementation contract. An otherwise valid health response may
			// still advertise an incompatible Office wrapper.
			Enabled:    officeCompatible,
			Formats:    append([]string(nil), health.Capabilities.Office.Formats...),
			DOCXGolden: officeDOCXGolden,
			PPTXGolden: officePPTXGolden,
			XLSXGolden: officeXLSXGolden,
		},
		PDF: config.RAGParserPDFSnapshot{
			// The approved ADR is scoped to one exact pypdfium2/PDFium
			// distribution. A schema-valid health response for another
			// engine or version must not publish PDF auto as available.
			Enabled:         pdfCompatible,
			LicenseApproved: c.pdfLicenseApproved,
		},
	}
	c.healthMu.Lock()
	c.health = cachedHealth{protocol: health, snapshot: snapshot}
	c.healthMu.Unlock()
	return snapshot, nil
}

func (c *Client) StartHealthProbe(ctx context.Context) {
	if c == nil {
		return
	}
	c.startOnce.Do(func() {
		go func() {
			c.probeHealthWithTimeout(ctx)
			ticker := time.NewTicker(c.healthProbeInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					c.probeHealthWithTimeout(ctx)
				}
			}
		}()
	})
}

func (c *Client) probeHealthWithTimeout(ctx context.Context) {
	timeout := c.timeout
	const maxHealthProbeTimeout = 5 * time.Second
	if timeout <= 0 || timeout > maxHealthProbeTimeout {
		timeout = maxHealthProbeTimeout
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, _ = c.ProbeHealth(probeCtx)
}

func (c *Client) currentHealth() (Health, config.RAGParserHealthSnapshot, bool) {
	if c == nil {
		return Health{}, config.RAGParserHealthSnapshot{}, false
	}
	c.healthMu.RLock()
	health := c.health.protocol
	snapshot := c.health.snapshot
	snapshot.Office.Formats = append([]string(nil), snapshot.Office.Formats...)
	c.healthMu.RUnlock()
	return health, snapshot, snapshot.Healthy && !snapshot.ExpiresAt.IsZero() && c.now().Before(snapshot.ExpiresAt)
}

func (c *Client) officeAvailable(format string) bool {
	health, _, fresh := c.currentHealth()
	if !fresh || !health.Capabilities.Office.Enabled {
		return false
	}
	if health.Capabilities.Office.MarkItDownVersion != ExpectedMarkItDownVersion ||
		health.Capabilities.Office.WrapperVersion != ExpectedOfficeWrapper {
		return false
	}
	for _, candidate := range health.Capabilities.Office.Formats {
		if candidate == format {
			return true
		}
	}
	return false
}

func (c *Client) pdfAvailable() bool {
	_, snapshot, fresh := c.currentHealth()
	return fresh && snapshot.PDF.Enabled && snapshot.PDF.LicenseApproved
}

func minPositive(left, right int64) int64 {
	if left <= 0 {
		return right
	}
	if right <= 0 || left < right {
		return left
	}
	return right
}

func normalizeSourceFormat(source document.Source) string {
	return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(source.Format)), ".")
}

func sourceMIMEType(format string) (string, error) {
	switch format {
	case "docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document", nil
	case "pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation", nil
	case "xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", nil
	case "pdf":
		return "application/pdf", nil
	default:
		return "", fmt.Errorf("unsupported parser source format %q", format)
	}
}

func (c *Client) validateSource(source document.Source, expectedFormats ...string) error {
	if err := source.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrSourceIntegrity, err)
	}
	format := normalizeSourceFormat(source)
	allowed := false
	for _, expected := range expectedFormats {
		if format == expected {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("unsupported parser source format %q", source.Format)
	}
	if source.Size <= 0 {
		return fmt.Errorf("%w: source is empty", ErrSourceLimitExceeded)
	}
	health, _, fresh := c.currentHealth()
	if !fresh {
		return unavailable(format, "health_unavailable_or_stale")
	}
	maxInput := minPositive(c.limits.MaxInputBytes, health.Limits.MaxInputBytes)
	if maxInput > 0 && source.Size > maxInput {
		return fmt.Errorf("%w: source bytes %d exceed %d", ErrSourceLimitExceeded, source.Size, maxInput)
	}
	extension := strings.TrimPrefix(strings.ToLower(filepath.Ext(source.FileName)), ".")
	if extension == "markdown" {
		extension = "md"
	}
	if extension != format {
		return fmt.Errorf("source format %q does not match file extension %q", format, extension)
	}
	if strings.ContainsAny(filepath.Base(source.FileName), "\r\n") {
		return errors.New("source file name contains an invalid header character")
	}
	return nil
}

func (c *Client) ConvertOffice(ctx context.Context, source document.Source) (*BundleHandle, error) {
	format := normalizeSourceFormat(source)
	if format != "docx" && format != "pptx" && format != "xlsx" {
		return nil, fmt.Errorf("unsupported Office source format %q", source.Format)
	}
	if !c.officeAvailable(format) {
		return nil, unavailable("office:"+format, "capability_unavailable")
	}
	if err := c.validateSource(source, "docx", "pptx", "xlsx"); err != nil {
		return nil, err
	}
	return c.postBundle(ctx, "office-convert", "/v1/office/convert", url.Values{"format": {format}}, source,
		DecodeOptions{ExpectedKind: BundleKindOfficeConvert})
}

func (c *Client) AnalyzePDF(ctx context.Context, source document.Source) (*BundleHandle, error) {
	if !c.pdfAvailable() {
		return nil, unavailable("pdf-analyze", "capability_or_license_unavailable")
	}
	if err := c.validateSource(source, "pdf"); err != nil {
		return nil, err
	}
	return c.postBundle(ctx, "pdf-analyze", "/v1/pdf/analyze", nil, source,
		DecodeOptions{ExpectedKind: BundleKindPDFAnalyze})
}

func (c *Client) RenderPDF(ctx context.Context, source document.Source, pages []int) (*BundleHandle, error) {
	if !c.pdfAvailable() {
		return nil, unavailable("pdf-render", "capability_or_license_unavailable")
	}
	if err := c.validateSource(source, "pdf"); err != nil {
		return nil, err
	}
	requested := append([]int(nil), pages...)
	sort.Ints(requested)
	if len(requested) == 0 {
		return nil, errors.New("pdf-render requires at least one page")
	}
	parts := make([]string, len(requested))
	for index, pageNumber := range requested {
		if pageNumber <= 0 || (index > 0 && requested[index-1] == pageNumber) {
			return nil, errors.New("pdf-render pages must be unique positive integers")
		}
		parts[index] = strconv.Itoa(pageNumber)
	}
	return c.postBundle(ctx, "pdf-render", "/v1/pdf/render", url.Values{"pages": {strings.Join(parts, ",")}}, source,
		DecodeOptions{ExpectedKind: BundleKindPDFRender, RequestedPages: requested})
}

func (c *Client) decodeLimits() DecodeLimits {
	health, _, _ := c.currentHealth()
	return DecodeLimits{
		MaxManifestBytes: c.limits.MaxManifestBytes,
		MaxEntries:       c.limits.MaxEntries,
		MaxPages:         c.limits.MaxPages,
		MaxAssets:        c.limits.MaxAssets,
		MaxImagePixels:   c.limits.MaxImagePixels,
		MaxEntryBytes:    c.limits.MaxEntryBytes,
		MaxAssetBytes:    c.limits.MaxAssetBytes,
		MaxRenderBytes:   c.limits.MaxRenderBytes,
		MaxTotalBytes:    c.limits.MaxExtractedBytes,
		MaxArchiveBytes:  minPositive(c.limits.MaxOutputBytes, health.Limits.MaxOutputBytes),
	}
}

func (c *Client) postBundle(
	ctx context.Context,
	operation, endpointPath string,
	query url.Values,
	source document.Source,
	decodeOptions DecodeOptions,
) (bundle *BundleHandle, resultErr error) {
	started := time.Now()
	defer func() {
		fields := telemetry.Fields{
			DocID: source.DocID, Format: normalizeSourceFormat(source), Operation: operation,
			Duration: time.Since(started), Outcome: "ok",
		}
		if bundle != nil {
			fields.PageCount = len(bundle.Manifest.Pages)
			fields.AssetCount = len(bundle.Manifest.Assets)
			fields.WarningCount = len(bundle.Manifest.Warnings)
			for _, occurrence := range bundle.Manifest.Occurrences {
				if occurrence.Decorative {
					fields.Decorative++
				}
			}
		}
		if resultErr != nil {
			fields.Outcome = "error"
			fields.ErrorCode = sidecarTelemetryErrorCode(resultErr)
		}
		telemetry.Emit(ctx, c.recorder, telemetry.EventParserSidecarCall, fields)
	}()
	pipeReader, pipeWriter := io.Pipe()
	multipartWriter := multipart.NewWriter(pipeWriter)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- writeMultipartSource(ctx, pipeWriter, multipartWriter, source)
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.requestURL(endpointPath, query), pipeReader)
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
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_ = pipeReader.CloseWithError(errors.New("sidecar rejected request"))
		<-writerDone
		return nil, &HTTPError{Operation: operation, StatusCode: response.StatusCode}
	}
	if writerErr := <-writerDone; writerErr != nil {
		return nil, writerErr
	}
	mediaType, parameters, parseErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if parseErr != nil || mediaType != "application/x-tar" || len(parameters) != 0 {
		return nil, invalidBundle("%s response Content-Type is %q", operation, response.Header.Get("Content-Type"))
	}
	decodeOptions.ExpectedSource = SourceDescriptor{
		Format: normalizeSourceFormat(source), ByteSize: source.Size, SHA256: source.SHA256,
	}
	decodeOptions.Limits = c.decodeLimits()
	decodeOptions.TempDir = c.tempDir
	return DecodeBundle(ctx, response.Body, decodeOptions)
}

func sidecarTelemetryErrorCode(err error) string {
	var httpErr *HTTPError
	switch {
	case errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusTooManyRequests:
		return "rate_limit"
	case errors.As(err, &httpErr) && httpErr.StatusCode >= 500:
		return "upstream"
	case errors.As(err, &httpErr):
		return "http_rejected"
	case errors.Is(err, ErrCapabilityUnavailable):
		return "capability_unavailable"
	case errors.Is(err, ErrInvalidBundle):
		return "invalid_bundle"
	case errors.Is(err, ErrBundleLimitExceeded), errors.Is(err, ErrSourceLimitExceeded):
		return "limit_exceeded"
	case errors.Is(err, ErrSourceIntegrity):
		return "source_integrity"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	default:
		return "sidecar_error"
	}
}

func writeMultipartSource(
	ctx context.Context,
	pipeWriter *io.PipeWriter,
	multipartWriter *multipart.Writer,
	source document.Source,
) (result error) {
	defer func() {
		if closeErr := multipartWriter.Close(); result == nil && closeErr != nil {
			result = closeErr
		}
		_ = pipeWriter.CloseWithError(result)
	}()
	reader, err := source.Open(ctx)
	if err != nil {
		return err
	}
	var closeOnce sync.Once
	var closeErr error
	closeReader := func() {
		closeOnce.Do(func() { closeErr = reader.Close() })
	}
	stopCancelClose := context.AfterFunc(ctx, closeReader)
	defer func() {
		_ = stopCancelClose()
		closeReader()
		if result == nil && closeErr != nil {
			result = closeErr
		}
	}()
	contentType, err := sourceMIMEType(normalizeSourceFormat(source))
	if err != nil {
		return err
	}
	partHeader := make(textproto.MIMEHeader)
	partHeader.Set("Content-Disposition", multipart.FileContentDisposition("file", filepath.Base(source.FileName)))
	partHeader.Set("Content-Type", contentType)
	part, err := multipartWriter.CreatePart(partHeader)
	if err != nil {
		return err
	}
	hasher := sha256.New()
	stream := &contextReader{ctx: ctx, r: reader}
	buffer := make([]byte, 32*1024)
	written, err := io.CopyBuffer(io.MultiWriter(part, hasher), &io.LimitedReader{R: stream, N: source.Size}, buffer)
	if err != nil {
		return err
	}
	if written != source.Size {
		return fmt.Errorf("%w: wrote %d bytes, expected %d", ErrSourceIntegrity, written, source.Size)
	}
	var extra [1]byte
	extraBytes, probeErr := stream.Read(extra[:])
	if extraBytes > 0 {
		return fmt.Errorf("%w: reopened source exceeds declared size %d", ErrSourceIntegrity, source.Size)
	}
	if probeErr != nil && !errors.Is(probeErr, io.EOF) {
		return probeErr
	}
	if probeErr == nil {
		return fmt.Errorf("%w: source reader made no progress after declared size", ErrSourceIntegrity)
	}
	if actual := hex.EncodeToString(hasher.Sum(nil)); actual != source.SHA256 {
		return fmt.Errorf("%w: SHA-256 changed while streaming", ErrSourceIntegrity)
	}
	return nil
}
