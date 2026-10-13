//go:build linux

package tools

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

// Use a finite, idle pipeline instead of scanning /proc. PID files let the
// regression assert that descendants stopped, not merely that Go stopped waiting.
func timeoutPipeline(dir string) string {
	stage := func(name, body string) string {
		return "sh -c " + shellQuote("echo $$ > "+shellQuote(filepath.Join(dir, name))+"; "+body)
	}
	return "echo $$ > " + shellQuote(filepath.Join(dir, "shell")) + "; " +
		stage("producer", "sleep 30 & echo $! > "+shellQuote(filepath.Join(dir, "sleeper"))+"; wait") + " | " +
		stage("sort", "sort -u") + " | " + stage("head", "head")
}

func recordedPIDs(dir string) []int {
	entries, _ := os.ReadDir(dir)
	var pids []int
	for _, entry := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, entry.Name()))
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
			pids = append(pids, pid)
		}
	}
	return pids
}

func liveRecordedPIDs(dir string) []int {
	var live []int
	for _, pid := range recordedPIDs(dir) {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(b)[strings.LastIndexByte(string(b), ')')+1:])
		// Container init may reap asynchronously. Zombies have already stopped
		// executing and closed their descriptors, so are not surviving workers.
		if len(fields) > 0 && fields[0] != "Z" && fields[0] != "X" {
			live = append(live, pid)
		}
	}
	return live
}

func cleanupRecordedPIDs(dir string) {
	for _, pid := range liveRecordedPIDs(dir) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

func foregroundTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry(t.TempDir(), t.TempDir())
	registerHostExec(r, nil, nil)
	t.Cleanup(r.Close)
	return r
}

func assertNoLiveRecordedPIDs(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for len(liveRecordedPIDs(dir)) > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if live := liveRecordedPIDs(dir); len(live) > 0 {
		t.Errorf("descendants still running after tool returned: %v", live)
	}
}

func TestForegroundExecTimeoutKillsPipeline(t *testing.T) {
	for _, tool := range []string{"exec", HostExecToolName} {
		t.Run(tool, func(t *testing.T) {
			r := foregroundTestRegistry(t)
			unrelated := exec.Command("sleep", "30")
			if err := unrelated.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = unrelated.Process.Kill(); _ = unrelated.Wait() }()
			dir := t.TempDir()
			defer cleanupRecordedPIDs(dir)
			// Also makes the old implementation fail in bounded time, without
			// leaving its orphaned pipeline running after the regression test.
			guard := time.AfterFunc(6*time.Second, func() { cleanupRecordedPIDs(dir) })
			defer guard.Stop()
			args, _ := json.Marshal(execArgs{Command: timeoutPipeline(dir), Timeout: 1})
			started := time.Now()
			_, err := r.Execute(context.Background(), tool, string(args))
			if elapsed := time.Since(started); elapsed > 4*time.Second {
				t.Errorf("one-second timeout took %v; output pipe remained open", elapsed)
			}
			if err == nil {
				t.Error("timed-out pipeline reported success")
			}
			if len(recordedPIDs(dir)) != 5 {
				t.Fatalf("pipeline did not start all five recorded processes")
			}
			assertNoLiveRecordedPIDs(t, dir)
			if err := unrelated.Process.Signal(syscall.Signal(0)); err != nil {
				t.Errorf("timeout killed an unrelated command: %v", err)
			}
		})
	}
}

func TestForegroundExecCancellationKillsPipeline(t *testing.T) {
	for _, tool := range []string{"exec", HostExecToolName} {
		t.Run(tool, func(t *testing.T) {
			r := foregroundTestRegistry(t)
			dir := t.TempDir()
			defer cleanupRecordedPIDs(dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			args, _ := json.Marshal(execArgs{Command: timeoutPipeline(dir), Timeout: 30})
			done := make(chan error, 1)
			go func() { _, err := r.Execute(ctx, tool, string(args)); done <- err }()
			deadline := time.Now().Add(3 * time.Second)
			for len(recordedPIDs(dir)) != 5 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("cancellation error = %v", err)
				}
			case <-time.After(4 * time.Second):
				cleanupRecordedPIDs(dir)
				<-done
				t.Fatal("cancelled call waited for descendants")
			}
			assertNoLiveRecordedPIDs(t, dir)
		})
	}
}

func TestForegroundExecExitedShellBoundsOutputWait(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			r := foregroundTestRegistry(t)
			dir := t.TempDir()
			defer cleanupRecordedPIDs(dir)
			guard := time.AfterFunc(6*time.Second, func() { cleanupRecordedPIDs(dir) })
			defer guard.Stop()
			pidFile := shellQuote(filepath.Join(dir, "child"))
			command := "sleep 30 & echo $! > " + pidFile + "; printf partial; exit " + strconv.Itoa(code)
			args, _ := json.Marshal(execArgs{Command: command, Timeout: 30})
			started := time.Now()
			out, err := r.Execute(context.Background(), "exec", string(args))
			if elapsed := time.Since(started); elapsed > 4*time.Second {
				t.Errorf("exited shell's pipe held tool open for %v", elapsed)
			}
			if code == 0 && !errors.Is(err, exec.ErrWaitDelay) {
				t.Errorf("expected output-wait error, got %v", err)
			}
			if code != 0 {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
					t.Errorf("lost shell exit status: %v", err)
				}
			}
			if !strings.Contains(out, "partial") {
				t.Errorf("lost partial output: %q", out)
			}
			assertNoLiveRecordedPIDs(t, dir)
		})
	}
}

func TestForegroundExecPreservesNormalResults(t *testing.T) {
	for _, tool := range []string{"exec", HostExecToolName} {
		t.Run(tool, func(t *testing.T) {
			r := foregroundTestRegistry(t)
			out, err := r.Execute(context.Background(), tool, `{"command":"printf out; printf err >&2"}`)
			if err != nil || out != "outerr" {
				t.Fatalf("output = %q, error = %v", out, err)
			}
			_, err = r.Execute(context.Background(), tool, `{"command":"exit 7"}`)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
				t.Errorf("lost command exit status: %v", err)
			}
		})
	}
}

// A process that deliberately starts a new session can escape group signalling.
// WaitDelay must still release the caller; this test cleans up that escapee itself.
func TestForegroundExecEscapedWriterCannotHoldOutput(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid unavailable")
	}
	r := foregroundTestRegistry(t)
	dir := t.TempDir()
	defer cleanupRecordedPIDs(dir)
	guard := time.AfterFunc(6*time.Second, func() { cleanupRecordedPIDs(dir) })
	defer guard.Stop()
	command := "setsid sh -c " + shellQuote("echo $$ > "+shellQuote(filepath.Join(dir, "escapee"))+"; exec sleep 30") + " & wait"
	args, _ := json.Marshal(execArgs{Command: command, Timeout: 1})
	started := time.Now()
	_, err := r.Execute(context.Background(), "exec", string(args))
	if elapsed := time.Since(started); elapsed > 4*time.Second {
		t.Errorf("escaped writer blocked return for %v", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected deadline error, got %v", err)
	}
	if len(liveRecordedPIDs(dir)) != 1 {
		t.Error("fixture did not retain an escaped writer to exercise the pipe wait limit")
	}
}
