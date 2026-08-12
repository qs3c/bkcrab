package imagegen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/qs3c/bkcrab/internal/workspace"
)

const artifactManifestVersion = 1

var (
	canonicalArtifactBatchID = regexp.MustCompile(`^imgb_[a-z0-9]{16,64}$`)
	canonicalArtifactTaskID  = regexp.MustCompile(`^imgt_[a-z0-9]{16,64}$`)
	artifactFingerprint      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	artifactBlockedNetworks  = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
		netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001:db8::/32"),
	}
)

type ArtifactLimits struct {
	MaxImageBytes     int64
	MaxBatchBytes     int64
	MaxPixels         int64
	RedirectLimit     int
	MaxManifestBytes  int64
	MaxCleanupObjects int
}

func (l ArtifactLimits) withDefaults() ArtifactLimits {
	if l.MaxImageBytes <= 0 {
		l.MaxImageBytes = 20 << 20
	}
	if l.MaxBatchBytes <= 0 {
		l.MaxBatchBytes = 128 << 20
	}
	if l.MaxPixels <= 0 {
		l.MaxPixels = 100_000_000
	}
	if l.RedirectLimit <= 0 {
		l.RedirectLimit = 3
	}
	if l.MaxManifestBytes <= 0 {
		l.MaxManifestBytes = 1 << 20
	}
	if l.MaxCleanupObjects <= 0 {
		l.MaxCleanupObjects = 1000
	}
	return l
}

type ArtifactScope struct {
	AgentID   string `json:"agent_id"`
	ProjectID string `json:"project_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

func (s ArtifactScope) validate() error {
	if !safeArtifactSegment(s.AgentID, false) || !safeArtifactSegment(s.ProjectID, true) || !safeArtifactSegment(s.SessionID, true) {
		return errors.New("imagegen: invalid artifact workspace scope")
	}
	return nil
}

type ImageArtifact struct {
	Index    int    `json:"index"`
	Key      string `json:"key"`
	MIMEType string `json:"mime_type"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type ArtifactManifest struct {
	Version            int             `json:"version"`
	Scope              ArtifactScope   `json:"scope"`
	BatchID            string          `json:"batch_id"`
	TaskID             string          `json:"task_id"`
	ClaimGeneration    int64           `json:"claim_generation"`
	RequestFingerprint string          `json:"request_fingerprint"`
	Provider           string          `json:"provider"`
	Model              string          `json:"model"`
	Artifacts          []ImageArtifact `json:"artifacts"`
	CreatedAt          time.Time       `json:"created_at"`
	ManifestKey        string          `json:"-"`
}

type ArtifactPublishRequest struct {
	Scope              ArtifactScope
	BatchID            string
	TaskID             string
	ClaimGeneration    int64
	RequestFingerprint string
	Provider           string
	Model              string
	ExpectedCount      int
	Images             []GeneratedImage
}

type ArtifactSalvageRequest struct {
	Scope                   ArtifactScope
	BatchID                 string
	TaskID                  string
	PreviousClaimGeneration int64
	RequestFingerprint      string
	ExpectedCount           int
	CancelRequested         bool
}

type ArtifactPublisherOptions struct {
	Store           workspace.Store
	HTTPClient      *http.Client
	DownloadTimeout time.Duration
	TrustedOrigins  []string
	Limits          ArtifactLimits
	Now             func() time.Time
}

type ArtifactPublisher struct {
	store          workspace.Store
	client         *http.Client
	trustedOrigins map[string]struct{}
	limits         ArtifactLimits
	now            func() time.Time
}

func NewArtifactPublisher(options ArtifactPublisherOptions) (*ArtifactPublisher, error) {
	if options.Store == nil {
		return nil, errors.New("imagegen: artifact workspace store is required")
	}
	trusted := make(map[string]struct{}, len(options.TrustedOrigins))
	for _, raw := range options.TrustedOrigins {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("imagegen: invalid trusted artifact origin")
		}
		trusted[strings.ToLower(parsed.Scheme+"://"+parsed.Host)] = struct{}{}
	}
	client := http.DefaultClient
	if options.HTTPClient != nil {
		client = options.HTTPClient
	}
	copyClient := *client
	downloadTimeout := options.DownloadTimeout
	if downloadTimeout <= 0 {
		downloadTimeout = 60 * time.Second
	}
	if copyClient.Timeout <= 0 || copyClient.Timeout > downloadTimeout {
		copyClient.Timeout = downloadTimeout
	}
	redirectLimit := options.Limits.withDefaults().RedirectLimit
	copyClient.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) > redirectLimit {
			return errors.New("imagegen: image redirect limit exceeded")
		}
		if err := validateArtifactRemoteURL(request.Context(), request.URL, trusted); err != nil {
			return err
		}
		return nil
	}
	if transport, ok := client.Transport.(*http.Transport); ok {
		copyClient.Transport = safeArtifactTransport(transport.Clone(), trusted)
	} else if client.Transport == nil {
		copyClient.Transport = safeArtifactTransport(http.DefaultTransport.(*http.Transport).Clone(), trusted)
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &ArtifactPublisher{store: options.Store, client: &copyClient, trustedOrigins: trusted, limits: options.Limits.withDefaults(), now: now}, nil
}

func CanonicalImageClaimPrefix(batchID, taskID string, claimGeneration int64) (string, error) {
	if !canonicalArtifactBatchID.MatchString(batchID) || !canonicalArtifactTaskID.MatchString(taskID) || claimGeneration < 1 {
		return "", errors.New("imagegen: invalid canonical artifact identity")
	}
	return path.Join("imagegen", batchID, taskID, "claims", strconv.FormatInt(claimGeneration, 10)), nil
}

func (p *ArtifactPublisher) Publish(ctx context.Context, request ArtifactPublishRequest) (ArtifactManifest, error) {
	if err := p.validatePublishRequest(request); err != nil {
		return ArtifactManifest{}, err
	}
	if existing, ok, err := p.Salvage(ctx, ArtifactSalvageRequest{
		Scope: request.Scope, BatchID: request.BatchID, TaskID: request.TaskID,
		PreviousClaimGeneration: request.ClaimGeneration, RequestFingerprint: request.RequestFingerprint,
		ExpectedCount: request.ExpectedCount,
	}); err != nil {
		return ArtifactManifest{}, err
	} else if ok {
		return existing, nil
	}
	prefix, _ := CanonicalImageClaimPrefix(request.BatchID, request.TaskID, request.ClaimGeneration)
	manifest := ArtifactManifest{
		Version: artifactManifestVersion, Scope: request.Scope, BatchID: request.BatchID, TaskID: request.TaskID,
		ClaimGeneration: request.ClaimGeneration, RequestFingerprint: request.RequestFingerprint,
		Provider: request.Provider, Model: request.Model, CreatedAt: p.now().UTC(),
		ManifestKey: path.Join(prefix, "manifest.json"),
	}
	var total int64
	for index, generated := range request.Images {
		data, err := p.materializeImage(ctx, generated)
		if err != nil {
			return ArtifactManifest{}, err
		}
		total += int64(len(data))
		if total > p.limits.MaxBatchBytes {
			return ArtifactManifest{}, errors.New("imagegen: artifact batch byte limit exceeded")
		}
		mimeType, width, height, extension, err := validateImageBytes(data, p.limits)
		if err != nil {
			return ArtifactManifest{}, err
		}
		digest := sha256.Sum256(data)
		hash := hex.EncodeToString(digest[:])
		key := path.Join(prefix, fmt.Sprintf("image-%d-%s.%s", index, hash, extension))
		if err := p.putImmutable(ctx, request.Scope, key, data, mimeType); err != nil {
			return ArtifactManifest{}, err
		}
		manifest.Artifacts = append(manifest.Artifacts, ImageArtifact{
			Index: index, Key: key, MIMEType: mimeType, Size: int64(len(data)), SHA256: hash, Width: width, Height: height,
		})
	}
	encoded, err := json.Marshal(manifest)
	if err != nil || int64(len(encoded)) > p.limits.MaxManifestBytes {
		return ArtifactManifest{}, errors.New("imagegen: artifact manifest is invalid or too large")
	}
	if err := p.putImmutable(ctx, request.Scope, manifest.ManifestKey, encoded, "application/json"); err != nil {
		return ArtifactManifest{}, err
	}
	return manifest, nil
}

func (p *ArtifactPublisher) validatePublishRequest(request ArtifactPublishRequest) error {
	if p == nil || p.store == nil || p.client == nil {
		return errors.New("imagegen: artifact publisher is not configured")
	}
	if err := request.Scope.validate(); err != nil {
		return err
	}
	if _, err := CanonicalImageClaimPrefix(request.BatchID, request.TaskID, request.ClaimGeneration); err != nil {
		return err
	}
	if !artifactFingerprint.MatchString(request.RequestFingerprint) || strings.TrimSpace(request.Provider) == "" || len(request.Provider) > 120 || len(request.Model) > 240 {
		return errors.New("imagegen: invalid artifact manifest metadata")
	}
	if request.ExpectedCount < 1 || request.ExpectedCount > 4 || len(request.Images) != request.ExpectedCount {
		return errors.New("imagegen: artifact image count must exactly match expected count")
	}
	return nil
}

func (p *ArtifactPublisher) materializeImage(ctx context.Context, generated GeneratedImage) ([]byte, error) {
	if (len(generated.Bytes) == 0) == (strings.TrimSpace(generated.SourceURL) == "") {
		return nil, errors.New("imagegen: generated image must have exactly one source")
	}
	if len(generated.Bytes) > 0 {
		if int64(len(generated.Bytes)) > p.limits.MaxImageBytes {
			return nil, errors.New("imagegen: image byte limit exceeded")
		}
		return append([]byte(nil), generated.Bytes...), nil
	}
	parsed, err := url.Parse(generated.SourceURL)
	if err != nil || validateArtifactRemoteURL(ctx, parsed, p.trustedOrigins) != nil {
		return nil, errors.New("imagegen: remote image URL is not allowed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, errors.New("imagegen: remote image request is invalid")
	}
	response, err := p.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("imagegen: remote image download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imagegen: remote image returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > p.limits.MaxImageBytes {
		return nil, errors.New("imagegen: image byte limit exceeded")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, p.limits.MaxImageBytes+1))
	if err != nil {
		return nil, errors.New("imagegen: remote image read failed")
	}
	if int64(len(data)) > p.limits.MaxImageBytes {
		return nil, errors.New("imagegen: image byte limit exceeded")
	}
	return data, nil
}

func validateImageBytes(data []byte, limits ArtifactLimits) (mimeType string, width, height int, extension string, err error) {
	if len(data) == 0 || int64(len(data)) > limits.MaxImageBytes {
		return "", 0, 0, "", errors.New("imagegen: invalid image byte size")
	}
	mimeType = http.DetectContentType(data)
	switch mimeType {
	case "image/png":
		extension = "png"
	case "image/jpeg":
		extension = "jpg"
	case "image/gif":
		extension = "gif"
	case "image/webp":
		extension = "webp"
	default:
		return "", 0, 0, "", errors.New("imagegen: payload magic is not a supported image")
	}
	width, height, err = decodeArtifactDimensions(data, mimeType)
	if err != nil || width < 1 || height < 1 || int64(width) > limits.MaxPixels/int64(height) {
		return "", 0, 0, "", errors.New("imagegen: invalid or excessive image dimensions")
	}
	return mimeType, width, height, extension, nil
}

func decodeArtifactDimensions(data []byte, mimeType string) (int, int, error) {
	if mimeType != "image/webp" {
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		return cfg.Width, cfg.Height, err
	}
	if len(data) < 30 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, errors.New("invalid webp")
	}
	switch string(data[12:16]) {
	case "VP8X":
		return 1 + int(data[24]) + int(data[25])<<8 + int(data[26])<<16,
			1 + int(data[27]) + int(data[28])<<8 + int(data[29])<<16, nil
	case "VP8L":
		if len(data) < 25 || data[20] != 0x2f {
			return 0, 0, errors.New("invalid webp lossless header")
		}
		width := 1 + int(data[21]) + (int(data[22]&0x3f) << 8)
		height := 1 + (int(data[22]) >> 6) + (int(data[23]) << 2) + (int(data[24]&0x0f) << 10)
		return width, height, nil
	case "VP8 ":
		if len(data) < 30 || data[23] != 0x9d || data[24] != 0x01 || data[25] != 0x2a {
			return 0, 0, errors.New("invalid webp lossy header")
		}
		return (int(data[26]) | int(data[27])<<8) & 0x3fff, (int(data[28]) | int(data[29])<<8) & 0x3fff, nil
	default:
		return 0, 0, errors.New("unsupported webp chunk")
	}
}

func (p *ArtifactPublisher) putImmutable(ctx context.Context, scope ArtifactScope, key string, data []byte, mimeType string) error {
	if info, err := p.store.Stat(ctx, scope.AgentID, scope.ProjectID, scope.SessionID, key); err == nil {
		if info.Size != int64(len(data)) {
			return errors.New("imagegen: immutable artifact collision")
		}
		reader, err := p.store.Get(ctx, scope.AgentID, scope.ProjectID, scope.SessionID, key)
		if err != nil {
			return err
		}
		existing, readErr := io.ReadAll(io.LimitReader(reader, int64(len(data))+1))
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil || !bytes.Equal(existing, data) {
			return errors.New("imagegen: immutable artifact collision")
		}
		return nil
	} else if !errors.Is(err, workspace.ErrNotFound) {
		return err
	}
	return p.store.Put(ctx, scope.AgentID, scope.ProjectID, scope.SessionID, key, bytes.NewReader(data), int64(len(data)), mimeType)
}

func (p *ArtifactPublisher) Salvage(ctx context.Context, request ArtifactSalvageRequest) (ArtifactManifest, bool, error) {
	if request.CancelRequested || request.PreviousClaimGeneration < 1 {
		return ArtifactManifest{}, false, nil
	}
	if err := request.Scope.validate(); err != nil || !artifactFingerprint.MatchString(request.RequestFingerprint) || request.ExpectedCount < 1 || request.ExpectedCount > 4 {
		return ArtifactManifest{}, false, nil
	}
	prefix, err := CanonicalImageClaimPrefix(request.BatchID, request.TaskID, request.PreviousClaimGeneration)
	if err != nil {
		return ArtifactManifest{}, false, nil
	}
	manifestKey := path.Join(prefix, "manifest.json")
	reader, err := p.store.Get(ctx, request.Scope.AgentID, request.Scope.ProjectID, request.Scope.SessionID, manifestKey)
	if errors.Is(err, workspace.ErrNotFound) {
		return ArtifactManifest{}, false, nil
	}
	if err != nil {
		return ArtifactManifest{}, false, err
	}
	encoded, readErr := io.ReadAll(io.LimitReader(reader, p.limits.MaxManifestBytes+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || int64(len(encoded)) > p.limits.MaxManifestBytes {
		return ArtifactManifest{}, false, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var manifest ArtifactManifest
	if decoder.Decode(&manifest) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return ArtifactManifest{}, false, nil
	}
	manifest.ManifestKey = manifestKey
	if !manifestMatchesSalvage(manifest, request, prefix, p.limits) {
		return ArtifactManifest{}, false, nil
	}
	for _, artifact := range manifest.Artifacts {
		info, err := p.store.Stat(ctx, request.Scope.AgentID, request.Scope.ProjectID, request.Scope.SessionID, artifact.Key)
		if err != nil || info.Size != artifact.Size {
			return ArtifactManifest{}, false, nil
		}
		object, err := p.store.Get(ctx, request.Scope.AgentID, request.Scope.ProjectID, request.Scope.SessionID, artifact.Key)
		if err != nil {
			return ArtifactManifest{}, false, nil
		}
		hash := sha256.New()
		read, copyErr := io.Copy(hash, io.LimitReader(object, artifact.Size+1))
		closeErr := object.Close()
		if copyErr != nil || closeErr != nil || read != artifact.Size || hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
			return ArtifactManifest{}, false, nil
		}
	}
	return manifest, true, nil
}

func manifestMatchesSalvage(manifest ArtifactManifest, request ArtifactSalvageRequest, prefix string, limits ArtifactLimits) bool {
	if manifest.Version != artifactManifestVersion || manifest.Scope != request.Scope || manifest.BatchID != request.BatchID || manifest.TaskID != request.TaskID ||
		manifest.ClaimGeneration != request.PreviousClaimGeneration || manifest.RequestFingerprint != request.RequestFingerprint || len(manifest.Artifacts) != request.ExpectedCount ||
		len(manifest.Artifacts) < 1 || len(manifest.Artifacts) > 4 || strings.TrimSpace(manifest.Provider) == "" || len(manifest.Provider) > 120 || len(manifest.Model) > 240 {
		return false
	}
	var total int64
	for index, artifact := range manifest.Artifacts {
		if artifact.Index != index || artifact.Size < 1 || artifact.Size > limits.MaxImageBytes || artifact.Width < 1 || artifact.Height < 1 ||
			int64(artifact.Width) > limits.MaxPixels/int64(artifact.Height) || !artifactFingerprint.MatchString(artifact.SHA256) {
			return false
		}
		total += artifact.Size
		if total > limits.MaxBatchBytes {
			return false
		}
		extension := extensionForMIME(artifact.MIMEType)
		if extension == "" || artifact.Key != path.Join(prefix, fmt.Sprintf("image-%d-%s.%s", index, artifact.SHA256, extension)) {
			return false
		}
	}
	return true
}

func (p *ArtifactPublisher) DeleteClaimArtifacts(ctx context.Context, manifest ArtifactManifest) error {
	if p == nil || p.store == nil || manifest.Scope.validate() != nil {
		return errors.New("imagegen: invalid artifact cleanup request")
	}
	prefix, err := CanonicalImageClaimPrefix(manifest.BatchID, manifest.TaskID, manifest.ClaimGeneration)
	if err != nil || manifest.ManifestKey != path.Join(prefix, "manifest.json") || !manifestMatchesSalvage(manifest, ArtifactSalvageRequest{
		Scope: manifest.Scope, BatchID: manifest.BatchID, TaskID: manifest.TaskID,
		PreviousClaimGeneration: manifest.ClaimGeneration, RequestFingerprint: manifest.RequestFingerprint,
		ExpectedCount: len(manifest.Artifacts),
	}, prefix, p.limits) {
		return errors.New("imagegen: invalid artifact cleanup manifest")
	}
	var errs []error
	for _, artifact := range manifest.Artifacts {
		if err := p.store.Delete(ctx, manifest.Scope.AgentID, manifest.Scope.ProjectID, manifest.Scope.SessionID, artifact.Key); err != nil {
			errs = append(errs, err)
		}
	}
	if manifest.ManifestKey != "" {
		if err := p.store.Delete(ctx, manifest.Scope.AgentID, manifest.Scope.ProjectID, manifest.Scope.SessionID, manifest.ManifestKey); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// CleanupStaleClaimArtifacts deletes claim objects for one task except the
// explicitly retained generations. Listing and deletion are bounded.
func (p *ArtifactPublisher) CleanupStaleClaimArtifacts(ctx context.Context, scope ArtifactScope, batchID, taskID string, retain map[int64]struct{}) (int, error) {
	if p == nil || p.store == nil || scope.validate() != nil || !canonicalArtifactBatchID.MatchString(batchID) || !canonicalArtifactTaskID.MatchString(taskID) {
		return 0, errors.New("imagegen: invalid stale claim cleanup request")
	}
	objects, err := p.store.List(ctx, scope.AgentID, scope.ProjectID, scope.SessionID)
	if err != nil {
		return 0, err
	}
	prefix := path.Join("imagegen", batchID, taskID, "claims") + "/"
	deleted := 0
	var errs []error
	for _, object := range objects {
		if !strings.HasPrefix(object.Path, prefix) {
			continue
		}
		remainder := strings.TrimPrefix(object.Path, prefix)
		generationText, _, found := strings.Cut(remainder, "/")
		generation, parseErr := strconv.ParseInt(generationText, 10, 64)
		if !found || parseErr != nil || generation < 1 {
			continue
		}
		if _, keep := retain[generation]; keep {
			continue
		}
		if deleted >= p.limits.MaxCleanupObjects {
			return deleted, errors.Join(append(errs, errors.New("imagegen: artifact cleanup bound reached"))...)
		}
		if err := p.store.Delete(ctx, scope.AgentID, scope.ProjectID, scope.SessionID, object.Path); err != nil {
			errs = append(errs, err)
		} else {
			deleted++
		}
	}
	return deleted, errors.Join(errs...)
}

func (p *ArtifactPublisher) DeleteBatchArtifacts(ctx context.Context, scope ArtifactScope, batchID string) (int, error) {
	if p == nil || p.store == nil || scope.validate() != nil || !canonicalArtifactBatchID.MatchString(batchID) {
		return 0, errors.New("imagegen: invalid batch artifact cleanup request")
	}
	objects, err := p.store.List(ctx, scope.AgentID, scope.ProjectID, scope.SessionID)
	if err != nil {
		return 0, err
	}
	prefix := path.Join("imagegen", batchID) + "/"
	deleted := 0
	var errs []error
	for _, object := range objects {
		if !strings.HasPrefix(object.Path, prefix) {
			continue
		}
		if deleted >= p.limits.MaxCleanupObjects {
			return deleted, errors.Join(append(errs, errors.New("imagegen: artifact cleanup bound reached"))...)
		}
		if err := p.store.Delete(ctx, scope.AgentID, scope.ProjectID, scope.SessionID, object.Path); err != nil {
			errs = append(errs, err)
		} else {
			deleted++
		}
	}
	return deleted, errors.Join(errs...)
}

func extensionForMIME(mimeType string) string {
	switch mimeType {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	default:
		return ""
	}
}

func safeArtifactSegment(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	return len(value) <= 191 && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n")
}

func artifactOrigin(parsed *url.URL) string {
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
}

func validateArtifactRemoteURL(ctx context.Context, parsed *url.URL, trusted map[string]struct{}) error {
	if parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return errors.New("imagegen: invalid remote image URL")
	}
	if _, ok := trusted[artifactOrigin(parsed)]; ok {
		return nil
	}
	if parsed.Scheme != "https" {
		return errors.New("imagegen: remote image URL must use HTTPS")
	}
	return validatePublicHost(ctx, parsed.Hostname())
}

func validatePublicHost(ctx context.Context, host string) error {
	if strings.EqualFold(host, "localhost") || host == "" {
		return errors.New("imagegen: private remote image host is not allowed")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(addresses) == 0 {
		return errors.New("imagegen: remote image host cannot be resolved")
	}
	for _, address := range addresses {
		if !publicArtifactIP(address.IP) {
			return errors.New("imagegen: private remote image host is not allowed")
		}
	}
	return nil
}

func publicArtifactIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, blocked := range artifactBlockedNetworks {
		if blocked.Contains(address) {
			return false
		}
	}
	return true
}

func safeArtifactTransport(transport *http.Transport, trusted map[string]struct{}) *http.Transport {
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("imagegen: invalid remote image address")
		}
		// Exact trusted origins are validated at the URL layer; retain the
		// client's normal resolver so local custom endpoints remain testable.
		trustedHost := false
		for origin := range trusted {
			parsed, _ := url.Parse(origin)
			originPort := parsed.Port()
			if originPort == "" {
				if parsed.Scheme == "https" {
					originPort = "443"
				} else {
					originPort = "80"
				}
			}
			if strings.EqualFold(parsed.Hostname(), host) && originPort == port {
				trustedHost = true
				break
			}
		}
		if trustedHost {
			return dialer.DialContext(ctx, network, address)
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, errors.New("imagegen: remote image host cannot be resolved")
		}
		for _, candidate := range addresses {
			if !publicArtifactIP(candidate.IP) {
				return nil, errors.New("imagegen: private remote image host is not allowed")
			}
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), port))
	}
	return transport
}
