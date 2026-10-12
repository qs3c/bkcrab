//go:build unix

package tools

// Regression coverage for the "shell timeout + retained process pipeline"
// incident (swe-live-20 / bkcrab / django__django-15022__RkmihL7):
//
//   grep -rho "django__django-[0-9]*" / 2>/dev/null | sort -u | head
//
// The synchronous exec tool ran the pipeline through `sh -c` with
// exec.CommandContext and CombinedOutput. When the tool timeout fired,
// Go's default CommandContext cancel killed only the direct `sh` child;
// the orphaned grep/sort/head pipeline (reparented to PID 1) kept the
// write end of the combined output pipe open forever. CombinedOutput's
// copier never saw EOF, so the tool call never returned, the agent loop
// stalled, and the harness eventually aborted the trial with
// AgentTimeoutError after 1800s. A human had to SIGTERM the orphans.
//
// The fix puts the synchronous shell in its own process group, kills the
// whole group on context cancellation, and bounds the post-exit pipe
// drain with Cmd.WaitDelay. These tests fail (hang or exceed the guard)
// on the pre-fix code and must stay green afterwards.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testExecResult captures an exec tool call's outcome from a helper
// goroutine so the test can enforce a wall-clock guard instead of
// blocking forever on the buggy code path.
type testExecResult struct {
	out string
	err error
}

func runExecToolWithGuard(t *testing.T, r *Registry, args map[string]any, guard time.Duration) (testExecResult, time.Duration) {
	t.Helper()
	raw, _ := json.Marshal(args)
	done := make(chan testExecResult, 1)
	start := time.Now()
	go func() {
		out, err := r.Execute(context.Background(), "exec", string(raw))
		done <- testExecResult{out: out, err: err}
	}()
	select {
	case got := <-done:
		return got, time.Since(start)
	case <-time.After(guard):
		t.Fatalf("exec did not return within %v — retained output pipe hung the tool (orphaned descendant kept stdout open)", guard)
		return testExecResult{}, 0
	}
}

// TestExecTool_TimeoutKillsProcessGroup reproduces the incident shape: a
// backgrounded descendant inherits the tool's stdout pipe and would keep
// it open past the timeout. The tool must return promptly at the timeout
// and the descendant must be dead — proof the cancellation reaches the
// whole process group, not just `sh`.
func TestExecTool_TimeoutKillsProcessGroup(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	defer r.Close()

	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// `sleep 300 &` inherits the combined stdout pipe. `echo $! > pidFile`
	// records the descendant so we can verify it was actually killed.
	command := fmt.Sprintf("sleep 300 & echo $! > %s; wait", shellQuote(pidFile))

	// Safety net: on the buggy path the helper goroutine may still be
	// blocked when the guard fires. Kill the recorded descendant so the
	// test process does not leak a 5-minute sleeper.
	t.Cleanup(func() {
		if pid, err := readPID(pidFile); err == nil && pid > 0 {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})

	got, elapsed := runExecToolWithGuard(t, r, map[string]any{
		"command": command,
		"timeout": 2,
	}, 15*time.Second)

	if got.err == nil {
		t.Fatalf("expected a timeout error, got nil (out=%q)", got.out)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("timeout took %v; expected prompt group-kill (no pipe-drain stall)", elapsed)
	}

	pid, err := readPID(pidFile)
	if err != nil {
		t.Fatalf("could not read descendant pid from %s: %v", pidFile, err)
	}
	if pid <= 0 {
		t.Fatalf("descendant pid %d is not valid", pid)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if processGone(pid) {
			break // killed and/or reaped
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant pid %d survived exec timeout — process group was not killed", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestExecTool_DescendantHoldingPipeDoesNotHang guards Cmd.WaitDelay: even
// when the shell exits normally but a backgrounded descendant inherits and
// holds stdout open, the tool must return (with ErrWaitDelay) instead of
// blocking until that descendant decides to exit.
func TestExecTool_DescendantHoldingPipeDoesNotHang(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	defer r.Close()

	got, elapsed := runExecToolWithGuard(t, r, map[string]any{
		// `sleep 30 &` holds the inherited stdout pipe for 30s while the
		// shell itself exits immediately.
		"command": "sleep 30 & echo launched",
		"timeout": 60,
	}, 15*time.Second)

	if !strings.Contains(got.out, "launched") {
		t.Errorf("expected partial output to survive, got %q", got.out)
	}
	if got.err == nil {
		t.Fatalf("expected ErrWaitDelay from a retained pipe, got nil (out=%q)", got.out)
	}
	if !errors.Is(got.err, exec.ErrWaitDelay) {
		t.Errorf("expected exec.ErrWaitDelay, got %v", got.err)
	}
	if elapsed > 10*time.Second {
		t.Errorf("exec returned after %v; WaitDelay should bound the drain", elapsed)
	}
}

// TestExecTool_CommandCompletesNormally is a control: the new process-group
// and WaitDelay wiring must not disturb ordinary commands or their output.
func TestExecTool_CommandCompletesNormally(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	defer r.Close()

	raw, _ := json.Marshal(map[string]any{"command": "printf 'hello\\n'"})
	out, err := r.Execute(context.Background(), "exec", string(raw))
	if err != nil {
		t.Fatalf("execute: %v (out=%q)", err, out)
	}
	if strings.TrimSpace(out) != "hello" {
		t.Errorf("got %q want %q", out, "hello")
	}
}

func readPID(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("parse pid %q: %w", string(b), err)
	}
	return pid, nil
}

// processGone reports whether pid is no longer a live process. It accepts
// both ESRCH (already reaped) and a kernel-observed zombie state, because in
// a container PID 1 may not reap reparented orphans promptly and a zombie is
// already dead for the purposes of "was the group kill delivered".
func processGone(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	// Linux exposes the state as the field after "comm" in /proc/<pid>/stat.
	// comm may contain spaces and parentheses, so anchor on the last ')'.
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		if i := strings.LastIndexByte(string(b), ')'); i >= 0 && i+2 < len(b) {
			switch b[i+2] {
			case 'Z', 'X': // zombie / dead
				return true
			}
		}
	}
	return false
}
