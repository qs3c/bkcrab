//go:build manual

// Run with:
//   E2B_API_KEY=... E2B_TEMPLATE=9zaxtyw620z6xititg11 \
//   BKCLAW_AGENT_ID=agt_ccc0add56d30398bcfa1 \
//   go test ./internal/sandbox/ -run TestE2BReproWithRealWorkspace -v -count=1 -tags=manual -timeout 6m
//
// Reproduces the live-daemon path: real workspace.Store + real per-agent skill
// dirs, going through pool.Get exactly as gateway/userspace.go does, against
// the actually-configured e2b template. Lets us see whether the Hydrate command
// fails at runtime due to bundle size or some other production-only condition
// that the empty-workspace probe missed.

package sandbox

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bkclaw-ai/bkclaw/internal/workspace"
)

func TestE2BReproWithRealWorkspace(t *testing.T) {
	apiKey := os.Getenv("E2B_API_KEY")
	template := os.Getenv("E2B_TEMPLATE")
	agentID := os.Getenv("BKCLAW_AGENT_ID")
	if apiKey == "" || template == "" || agentID == "" {
		t.Skip("set E2B_API_KEY, E2B_TEMPLATE, BKCLAW_AGENT_ID")
	}
	home := os.Getenv("BKCLAW_HOME")
	if home == "" {
		home = filepath.Join(os.Getenv("HOME"), ".bkclaw")
	}

	ws := workspace.NewLocalFS(filepath.Join(home, "workspaces"))

	// Quick survey of how much data Hydrate will ship in the bundle, so
	// when the bundle is huge we know that's what to blame.
	objs, err := ws.List(context.Background(), agentID, "", "")
	if err != nil {
		t.Logf("workspace.List error: %v", err)
	} else {
		var total int64
		for _, o := range objs {
			total += o.Size
		}
		t.Logf("workspace has %d objs, %d bytes raw", len(objs), total)
	}

	// Use a non-existent home so no skill dirs are picked up and the bundle
	// stays empty — proves whether the bug is bundle-size related.
	useEmpty := os.Getenv("EMPTY_BUNDLE") == "1"
	homeForPool := home
	wsForPool := ws
	if useEmpty {
		homeForPool = "/nonexistent-for-test"
		wsForPool = nil
	}
	pool := NewE2BExecutorPool(apiKey, template, homeForPool, 5*time.Minute)
	if wsForPool != nil {
		pool.SetWorkspace(wsForPool)
	}
	defer pool.CloseAll()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	t.Logf("calling pool.Get(agent=%s) — same call path as the web UI (empty=%v)", agentID, useEmpty)
	exIfc, err := pool.Get(ctx, agentID, "", "sess-repro")
	if err != nil {
		t.Fatalf("pool.Get returned error: %v", err)
	}
	ex := exIfc.(*E2BExecutor)
	t.Logf("got healthy sandbox %s", ex.sandboxID)

	// Repeat the write_file the user was attempting.
	out, err := ex.WriteFile(ctx, "/workspace/fib.py", "print('hi')\n")
	if err != nil {
		t.Fatalf("write_file failed (this is the live bug): %v", err)
	}
	t.Logf("write_file ok: %s", out)
}

var _ = fmt.Sprintf
