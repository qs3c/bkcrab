package sandbox

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/workspace"
)

// Opt-in: run inside the deployed gateway environment. Uses isolated agent and
// S3 prefix names, no user records, LLM calls or production object mutations.
func TestDockerMinioQuotaIntegration(t *testing.T) {
	if os.Getenv("BKCRAB_INTEGRATION_DOCKER") != "1" {
		t.Skip("requires real DinD and MinIO")
	}
	ctx := context.Background()
	const agent = "quota_integration_agent"
	const owner = "quota_integration_user"
	home := os.Getenv("BKCRAB_HOME")
	if home == "" || os.Getenv("BKCRAB_SANDBOX_QUOTA_IMAGE") == "" {
		t.Fatal("quota-enabled gateway environment required")
	}
	ws, err := workspace.NewS3(workspace.S3Config{Endpoint: os.Getenv("BKCRAB_OBJECT_STORE_ENDPOINT"), Bucket: os.Getenv("BKCRAB_OBJECT_STORE_BUCKET"), AccessKey: os.Getenv("BKCRAB_OBJECT_STORE_ACCESSKEY"), SecretKey: os.Getenv("BKCRAB_OBJECT_STORE_SECRETKEY"), Prefix: "reliability-integration/" + time.Now().UTC().Format("20060102T150405.000000000")})
	if err != nil {
		t.Fatal(err)
	}
	inner := NewDockerExecutorPool("qs3c/bkcrab-sandbox:latest", home, &Policy{MaxCPU: "1", MaxMemory: "2g", MaxPIDs: 256, ProtectQuota: true})
	inner.ConfigureQuota(Limits{QuotaImage: os.Getenv("BKCRAB_SANDBOX_QUOTA_IMAGE"), WorkspaceBytes: 4 << 20, WorkspaceFiles: 100}, func(context.Context, string) (string, error) { return owner, nil })
	p := NewLifecyclePool(inner, time.Minute, time.Minute)
	p.SetWorkspace(ws)
	p.SetLimits(Limits{MaxContainers: 1, MaxPerUser: 1, QueueTimeout: time.Second})
	p.Start()
	defer func() {
		p.CloseAll()
		objects, _ := ws.List(ctx, agent, "", "")
		for _, o := range objects {
			if err := ws.Delete(ctx, agent, "", "", o.Path); err != nil {
				t.Errorf("cleanup object: %v", err)
			}
		}
		// Only the fixed, isolated test agent's directory is removed.
		if err := os.RemoveAll(filepath.Join(home, "workspaces", agent)); err != nil {
			t.Errorf("cleanup workspace: %v", err)
		}
	}()
	e, _ := p.Get(WithUserID(ctx, owner), agent, "", "")
	for _, content := range []string{"first", "other"} {
		if out, err := e.Exec(ctx, "printf "+content+" > persisted.txt", 10*time.Second); err != nil {
			t.Fatalf("exec: %s: %v", out, err)
		}
		r, err := ws.Get(ctx, agent, "", "", "persisted.txt")
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(data) != content {
			t.Fatalf("MinIO round trip: %q %v", data, err)
		}
	}
	out, err := e.Exec(ctx, "dd if=/dev/zero of=quota-test bs=1M count=8", 10*time.Second)
	if err == nil || !strings.Contains(strings.ToLower(out), "quota exceeded") {
		t.Fatalf("hard quota did not reject: %s %v", out, err)
	}
	out, err = e.Exec(ctx, "chattr -p 0 /workspace", 10*time.Second)
	if err == nil || !strings.Contains(out, "Operation not permitted") {
		t.Fatalf("project quota bypass not blocked: %s %v", out, err)
	}
}
