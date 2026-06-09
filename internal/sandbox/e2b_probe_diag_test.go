//go:build manual

// Run with: go test ./internal/sandbox/ -run TestE2BProbeWorkspacePerms -v -count=1 -tags=manual
//
// This is a manual diagnostic: it spins up a real E2B sandbox against the
// configured template+key, runs a battery of identity/permission checks
// against /workspace, then ALSO replays the chown step the production
// Hydrate path does and re-checks. Lets us answer "does Hydrate's chown
// actually work, and what user owns /workspace before/after?".
//
// Build tag keeps it out of normal `go test ./...` runs — it hits a
// paid API and creates a real sandbox.

package sandbox

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestE2BProbeWorkspacePerms(t *testing.T) {
	apiKey := os.Getenv("E2B_API_KEY")
	template := os.Getenv("E2B_TEMPLATE")
	if apiKey == "" || template == "" {
		t.Skip("set E2B_API_KEY and E2B_TEMPLATE env vars")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ex, err := newE2BExecutor(ctx, apiKey, template, 5*time.Minute)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	t.Logf("sandbox: %s (template=%s)", ex.sandboxID, ex.template)

	// 1. Identity + workspace state BEFORE hydrate runs anything.
	t.Log("--- BEFORE hydrate ---")
	probe(t, ex, "whoami")
	probe(t, ex, "id")
	probe(t, ex, "ls -ld /workspace 2>&1 || echo 'NO /workspace'")
	probe(t, ex, "ls -ld /tmp")
	probe(t, ex, "sudo -n true 2>&1 && echo 'has-sudo-nopasswd' || echo 'no-sudo'")
	probe(t, ex, "touch /workspace/probe-before 2>&1; ls /workspace/probe-before 2>&1 || true")

	// 2. Now run the EXACT mkdir+chown sequence Hydrate runs.
	t.Log("--- replaying Hydrate's mkdir+chown ---")
	probe(t, ex, "sudo mkdir -p /workspace; sudo chown user:user /workspace; ls -ld /workspace")

	// 3. Same battery AFTER. If the chown didn't stick we'll see it here.
	t.Log("--- AFTER hydrate-style chown ---")
	probe(t, ex, "ls -ld /workspace")
	probe(t, ex, "touch /workspace/probe-after 2>&1 && ls -l /workspace/probe-after")
	// Same operation the agent kept failing on:
	probe(t, ex, "python3 -c \"open('/workspace/probe-python.md','w').write('hello')\" 2>&1 && ls -l /workspace/probe-python.md")

	// 4. cd /workspace prefix that production Exec() wraps with.
	t.Log("--- production-equivalent exec (cd /workspace && …) ---")
	wrapped, err := ex.Exec(ctx, "whoami; touch ws-relative.md && ls -l ws-relative.md; pwd", 15*time.Second)
	if err != nil {
		t.Errorf("wrapped exec failed: %v", err)
	}
	t.Logf("output:\n%s", wrapped)
}

// TestE2BProbePoolHealth exercises the full pool path with the new
// fail-fast Hydrate + health probe. It should leave a usable sandbox
// cached (touch /workspace works without any extra setup) — proving
// the change actually closes the broken-sandbox window.
func TestE2BProbePoolHealth(t *testing.T) {
	apiKey := os.Getenv("E2B_API_KEY")
	template := os.Getenv("E2B_TEMPLATE")
	if apiKey == "" || template == "" {
		t.Skip("set E2B_API_KEY and E2B_TEMPLATE env vars")
	}
	pool := NewE2BExecutorPool(apiKey, template, "", 5*time.Minute)
	defer pool.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	exIfc, err := pool.Get(ctx, "agt-probe", "", "sess-probe")
	if err != nil {
		t.Fatalf("Pool.Get returned error (this means health probe caught a bad sandbox or e2b is down): %v", err)
	}
	ex := exIfc.(*E2BExecutor)
	t.Logf("pool returned healthy sandbox %s", ex.sandboxID)

	// Without any setup of our own, /workspace must be agent-writable.
	out, err := ex.Exec(ctx, "touch deliverable.txt && ls -l deliverable.txt", 15*time.Second)
	if err != nil {
		t.Fatalf("post-Get /workspace write failed: %v", err)
	}
	t.Logf("agent-style write succeeded: %s", strings.TrimSpace(out))
}

func probe(t *testing.T, ex *E2BExecutor, cmd string) {
	t.Helper()
	// execOnce so we DON'T get the production "cd /workspace && " prefix,
	// which would itself fail if /workspace was missing.
	out, err := ex.execOnce(context.Background(), cmd, 15*time.Second)
	out = strings.TrimRight(out, "\n")
	if err != nil {
		t.Logf("$ %s\n  ERR: %v\n  OUT: %s", cmd, err, out)
		return
	}
	t.Logf("$ %s\n  %s", cmd, indent(out))
}

func indent(s string) string {
	return strings.ReplaceAll(s, "\n", "\n  ")
}

var _ = fmt.Sprintf // keep the import shaped right
