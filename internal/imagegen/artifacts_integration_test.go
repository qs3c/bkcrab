package imagegen

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/workspace"
)

func TestArtifactLocalFSIntegrationSalvageAndBoundedCleanup(t *testing.T) {
	local := workspace.NewLocalFS(t.TempDir())
	publisher, err := NewArtifactPublisher(ArtifactPublisherOptions{
		Store:  local,
		Limits: ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 4 << 20, MaxPixels: 1_000_000, MaxCleanupObjects: 20},
	})
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	scope := ArtifactScope{AgentID: "agent-a", SessionID: "session-a"}
	request := validArtifactPublishRequest([]GeneratedImage{{Bytes: artifactPNG(t, 2, 2)}})
	request.Scope = scope
	request.ClaimGeneration = 2
	claimTwo, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish claim 2: %v", err)
	}
	request.ClaimGeneration = 3
	claimThree, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish claim 3: %v", err)
	}
	if err := local.Put(context.Background(), scope.AgentID, scope.ProjectID, scope.SessionID, "unrelated.txt", bytes.NewBufferString("keep"), 4, "text/plain"); err != nil {
		t.Fatalf("put unrelated: %v", err)
	}

	deleted, err := publisher.CleanupStaleClaimArtifacts(context.Background(), scope, request.BatchID, request.TaskID, map[int64]struct{}{3: {}})
	if err != nil || deleted != 2 {
		t.Fatalf("cleanup stale: deleted=%d err=%v", deleted, err)
	}
	if _, err := local.Stat(context.Background(), scope.AgentID, scope.ProjectID, scope.SessionID, claimTwo.ManifestKey); !errors.Is(err, workspace.ErrNotFound) {
		t.Fatalf("stale manifest remains: %v", err)
	}
	if _, err := local.Stat(context.Background(), scope.AgentID, scope.ProjectID, scope.SessionID, claimThree.ManifestKey); err != nil {
		t.Fatalf("retained manifest missing: %v", err)
	}

	deleted, err = publisher.DeleteBatchArtifacts(context.Background(), scope, request.BatchID)
	if err != nil || deleted != 2 {
		t.Fatalf("delete batch: deleted=%d err=%v", deleted, err)
	}
	if info, err := local.Stat(context.Background(), scope.AgentID, scope.ProjectID, scope.SessionID, "unrelated.txt"); err != nil || info.Size != 4 {
		t.Fatalf("batch cleanup touched unrelated object: info=%#v err=%v", info, err)
	}
}

func TestArtifactSalvageRejectsTamperedObject(t *testing.T) {
	local := workspace.NewLocalFS(t.TempDir())
	publisher, err := NewArtifactPublisher(ArtifactPublisherOptions{Store: local, Limits: ArtifactLimits{MaxImageBytes: 1 << 20, MaxBatchBytes: 2 << 20, MaxPixels: 1_000_000}})
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	request := validArtifactPublishRequest([]GeneratedImage{{Bytes: artifactPNG(t, 2, 2)}})
	manifest, err := publisher.Publish(context.Background(), request)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	tampered := bytes.Repeat([]byte{'x'}, int(manifest.Artifacts[0].Size))
	if err := local.Put(context.Background(), request.Scope.AgentID, request.Scope.ProjectID, request.Scope.SessionID, manifest.Artifacts[0].Key, bytes.NewReader(tampered), int64(len(tampered)), "image/png"); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	_, ok, err := publisher.Salvage(context.Background(), ArtifactSalvageRequest{
		Scope: request.Scope, BatchID: request.BatchID, TaskID: request.TaskID, PreviousClaimGeneration: request.ClaimGeneration,
		RequestFingerprint: request.RequestFingerprint, ExpectedCount: request.ExpectedCount,
	})
	if err != nil || ok {
		t.Fatalf("tampered object salvaged: ok=%v err=%v", ok, err)
	}
}

func TestArtifactCleanupRejectsUntrustedManifestPaths(t *testing.T) {
	publisher, _ := artifactTestPublisher(t, ArtifactLimits{})
	manifest := ArtifactManifest{
		Version: artifactManifestVersion, Scope: ArtifactScope{AgentID: "agent-a"},
		BatchID: "imgb_0123456789abcdef", TaskID: "imgt_0123456789abcdef", ClaimGeneration: 1,
		RequestFingerprint: strings.Repeat("a", 64), ManifestKey: "unrelated.txt",
		Artifacts: []ImageArtifact{{Index: 0, Key: "unrelated.txt", MIMEType: "image/png", Size: 1, SHA256: strings.Repeat("a", 64), Width: 1, Height: 1}},
	}
	if err := publisher.DeleteClaimArtifacts(context.Background(), manifest); err == nil {
		t.Fatal("untrusted cleanup paths accepted")
	}
}
