package imagegen

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/workspace"
)

func artifactPNG(t *testing.T, width, height int) []byte {
	t.Helper()
	value := image.NewRGBA(image.Rect(0, 0, width, height))
	value.Set(0, 0, color.RGBA{R: 255, A: 255})
	var buffer bytes.Buffer
	if err := png.Encode(&buffer, value); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buffer.Bytes()
}

type recordingWorkspace struct {
	workspace.Store
	mu   sync.Mutex
	puts []string
}

func (s *recordingWorkspace) Put(ctx context.Context, agentID, projectID, sessionID, path string, reader io.Reader, size int64, contentType string) error {
	s.mu.Lock()
	s.puts = append(s.puts, path)
	s.mu.Unlock()
	return s.Store.Put(ctx, agentID, projectID, sessionID, path, reader, size, contentType)
}

func artifactTestPublisher(t *testing.T, limits ArtifactLimits) (*ArtifactPublisher, *recordingWorkspace) {
	t.Helper()
	store := &recordingWorkspace{Store: workspace.NewLocalFS(t.TempDir())}
	publisher, err := NewArtifactPublisher(ArtifactPublisherOptions{Store: store, Limits: limits, Now: func() time.Time {
		return time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	}})
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	return publisher, store
}

func validArtifactPublishRequest(images []GeneratedImage) ArtifactPublishRequest {
	return ArtifactPublishRequest{
		Scope:   ArtifactScope{AgentID: "agent-a", ProjectID: "project-a", SessionID: "session-a"},
		BatchID: "imgb_0123456789abcdef", TaskID: "imgt_0123456789abcdef", ClaimGeneration: 3,
		RequestFingerprint: strings.Repeat("a", 64), Provider: "openai", Model: "gpt-image-1",
		ExpectedCount: len(images), Images: images,
	}
}

func TestArtifactCanonicalKeysRejectTraversal(t *testing.T) {
	prefix, err := CanonicalImageClaimPrefix("imgb_0123456789abcdef", "imgt_0123456789abcdef", 3)
	if err != nil || prefix != "imagegen/imgb_0123456789abcdef/imgt_0123456789abcdef/claims/3" {
		t.Fatalf("prefix=%q err=%v", prefix, err)
	}
	for _, invalid := range [][2]string{{"../batch", "imgt_0123456789abcdef"}, {"imgb_0123456789abcdef", "../../task"}} {
		if _, err := CanonicalImageClaimPrefix(invalid[0], invalid[1], 3); err == nil {
			t.Fatalf("traversal accepted: %#v", invalid)
		}
	}
	if _, err := CanonicalImageClaimPrefix("imgb_0123456789abcdef", "imgt_0123456789abcdef", 0); err == nil {
		t.Fatal("zero claim generation accepted")
	}
}

func TestArtifactPublishValidatesAndWritesManifestLast(t *testing.T) {
	publisher, store := artifactTestPublisher(t, ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 1_000_000})
	request := validArtifactPublishRequest([]GeneratedImage{
		{Bytes: artifactPNG(t, 2, 3)}, {Bytes: artifactPNG(t, 4, 5), MIMEType: "application/octet-stream"},
	})
	manifest, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(manifest.Artifacts) != 2 || manifest.Artifacts[0].Width != 2 || manifest.Artifacts[1].Height != 5 {
		t.Fatalf("manifest artifacts: %#v", manifest.Artifacts)
	}
	if len(store.puts) != 3 || store.puts[2] != manifest.ManifestKey || !strings.HasSuffix(store.puts[2], "/manifest.json") {
		t.Fatalf("manifest was not written last: %#v", store.puts)
	}
	for _, artifact := range manifest.Artifacts {
		if !strings.Contains(artifact.Key, artifact.SHA256) || !strings.HasSuffix(artifact.Key, ".png") {
			t.Fatalf("non-canonical artifact key: %#v", artifact)
		}
	}
	store.mu.Lock()
	putsBeforeRetry := len(store.puts)
	store.mu.Unlock()
	if repeated, err := publisher.Publish(context.Background(), request); err != nil || repeated.ManifestKey != manifest.ManifestKey {
		t.Fatalf("idempotent publish: manifest=%#v err=%v", repeated, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.puts) != putsBeforeRetry {
		t.Fatalf("idempotent publish rewrote immutable objects: before=%d after=%d", putsBeforeRetry, len(store.puts))
	}
}

func TestArtifactPublishEnforcesImageAndBatchCaps(t *testing.T) {
	imageBytes := artifactPNG(t, 8, 8)
	tests := []struct {
		name   string
		limits ArtifactLimits
		images []GeneratedImage
	}{
		{name: "single", limits: ArtifactLimits{MaxImageBytes: int64(len(imageBytes) - 1), MaxBatchBytes: 1 << 20, MaxPixels: 1_000_000}, images: []GeneratedImage{{Bytes: imageBytes}}},
		{name: "batch", limits: ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: int64(len(imageBytes)*2 - 1), MaxPixels: 1_000_000}, images: []GeneratedImage{{Bytes: imageBytes}, {Bytes: imageBytes}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publisher, _ := artifactTestPublisher(t, tt.limits)
			if _, err := publisher.Publish(context.Background(), validArtifactPublishRequest(tt.images)); err == nil {
				t.Fatal("size cap violation accepted")
			}
		})
	}
}

func TestArtifactPublishRejectsInvalidMagicPixelsAndCount(t *testing.T) {
	tests := []struct {
		name    string
		limits  ArtifactLimits
		request ArtifactPublishRequest
	}{
		{name: "invalid magic", limits: ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 100}, request: validArtifactPublishRequest([]GeneratedImage{{Bytes: []byte("not an image"), MIMEType: "image/png"}})},
		{name: "pixels", limits: ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 3}, request: validArtifactPublishRequest([]GeneratedImage{{Bytes: artifactPNG(t, 2, 2)}})},
	}
	countMismatch := validArtifactPublishRequest([]GeneratedImage{{Bytes: artifactPNG(t, 1, 1)}})
	countMismatch.ExpectedCount = 2
	tests = append(tests, struct {
		name    string
		limits  ArtifactLimits
		request ArtifactPublishRequest
	}{name: "count mismatch", limits: ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 100}, request: countMismatch})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			publisher, _ := artifactTestPublisher(t, tt.limits)
			if _, err := publisher.Publish(context.Background(), tt.request); err == nil {
				t.Fatal("invalid artifact request accepted")
			}
		})
	}
}

func TestArtifactDownloaderRejectsSSRFAndBoundsRedirects(t *testing.T) {
	publisher, err := NewArtifactPublisher(ArtifactPublisherOptions{
		Store:  workspace.NewLocalFS(t.TempDir()),
		Limits: ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 1_000_000, RedirectLimit: 1},
	})
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	secretURL := "https://127.0.0.1/private.png?signature=do-not-log"
	_, err = publisher.Publish(context.Background(), validArtifactPublishRequest([]GeneratedImage{{SourceURL: secretURL}}))
	if err == nil || strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "do-not-log") {
		t.Fatalf("SSRF/query redaction: %v", err)
	}

	redirects := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects++
		http.Redirect(w, r, "/again", http.StatusFound)
	}))
	defer server.Close()
	trusted, err := NewArtifactPublisher(ArtifactPublisherOptions{
		Store: workspace.NewLocalFS(t.TempDir()), HTTPClient: server.Client(), TrustedOrigins: []string{server.URL},
		Limits: ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 1_000_000, RedirectLimit: 1},
	})
	if err != nil {
		t.Fatalf("trusted publisher: %v", err)
	}
	if _, err := trusted.Publish(context.Background(), validArtifactPublishRequest([]GeneratedImage{{SourceURL: server.URL + "/start?token=hidden"}})); err == nil {
		t.Fatal("redirect loop accepted")
	}
	if redirects > 2 {
		t.Fatalf("redirect bound exceeded: %d", redirects)
	}
}

func TestArtifactDownloaderAllowsExplicitTrustedHTTPSAndUsesMagic(t *testing.T) {
	pngBytes := artifactPNG(t, 3, 7)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(pngBytes)
	}))
	defer server.Close()
	publisher, err := NewArtifactPublisher(ArtifactPublisherOptions{
		Store: workspace.NewLocalFS(t.TempDir()), HTTPClient: server.Client(), TrustedOrigins: []string{server.URL},
		Limits: ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 1_000_000},
	})
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	manifest, err := publisher.Publish(context.Background(), validArtifactPublishRequest([]GeneratedImage{{SourceURL: server.URL + "/image?signed=hidden"}}))
	if err != nil {
		t.Fatalf("trusted download: %v", err)
	}
	if manifest.Artifacts[0].MIMEType != "image/png" || manifest.Artifacts[0].Width != 3 || manifest.Artifacts[0].Height != 7 {
		t.Fatalf("magic/dimensions: %#v", manifest.Artifacts[0])
	}
}

func TestArtifactDownloaderEnforcesTotalTimeout(t *testing.T) {
	pngBytes := artifactPNG(t, 1, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(80 * time.Millisecond)
		_, _ = w.Write(pngBytes)
	}))
	defer server.Close()
	publisher, err := NewArtifactPublisher(ArtifactPublisherOptions{
		Store: workspace.NewLocalFS(t.TempDir()), HTTPClient: server.Client(), TrustedOrigins: []string{server.URL},
		DownloadTimeout: 15 * time.Millisecond,
		Limits:          ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 1_000_000},
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = publisher.Publish(context.Background(), validArtifactPublishRequest([]GeneratedImage{{SourceURL: server.URL + "/slow"}}))
	if err == nil || time.Since(started) >= 70*time.Millisecond {
		t.Fatalf("download total timeout was not enforced: elapsed=%s err=%v", time.Since(started), err)
	}
}

func TestArtifactSalvageUsesOnlyExplicitPreviousClaimAndVerifiesObjects(t *testing.T) {
	publisher, _ := artifactTestPublisher(t, ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 1_000_000})
	request := validArtifactPublishRequest([]GeneratedImage{{Bytes: artifactPNG(t, 2, 2)}})
	manifest, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	salvaged, ok, err := publisher.Salvage(context.Background(), ArtifactSalvageRequest{
		Scope: request.Scope, BatchID: request.BatchID, TaskID: request.TaskID,
		PreviousClaimGeneration: request.ClaimGeneration, RequestFingerprint: request.RequestFingerprint, ExpectedCount: 1,
	})
	if err != nil || !ok || salvaged.ManifestKey != manifest.ManifestKey {
		t.Fatalf("salvage=%#v ok=%v err=%v", salvaged, ok, err)
	}
	for _, miss := range []ArtifactSalvageRequest{
		{Scope: request.Scope, BatchID: request.BatchID, TaskID: request.TaskID, PreviousClaimGeneration: 0, RequestFingerprint: request.RequestFingerprint, ExpectedCount: 1},
		{Scope: request.Scope, BatchID: request.BatchID, TaskID: request.TaskID, PreviousClaimGeneration: request.ClaimGeneration, RequestFingerprint: strings.Repeat("b", 64), ExpectedCount: 1},
		{Scope: request.Scope, BatchID: request.BatchID, TaskID: request.TaskID, PreviousClaimGeneration: request.ClaimGeneration, RequestFingerprint: request.RequestFingerprint, ExpectedCount: 1, CancelRequested: true},
	} {
		if _, ok, err := publisher.Salvage(context.Background(), miss); err != nil || ok {
			t.Fatalf("salvage miss accepted: ok=%v err=%v request=%#v", ok, err, miss)
		}
	}
	if err := publisher.DeleteClaimArtifacts(context.Background(), manifest); err != nil {
		t.Fatalf("delete claim: %v", err)
	}
	if _, err := publisher.store.Stat(context.Background(), request.Scope.AgentID, request.Scope.ProjectID, request.Scope.SessionID, manifest.ManifestKey); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("manifest still exists after cleanup: %v", err)
	}
}
