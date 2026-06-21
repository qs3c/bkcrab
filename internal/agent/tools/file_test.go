package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qs3c/bkclaw/internal/memory"
)

// TestApplyEdit pins the contract that edit_file's three backends share:
// a single match replaces in place, the empty / equal / not-found / multi-
// match cases each error with a fragment the LLM can act on, and
// replace_all swaps every occurrence. Pure-function tests only — backend
// routing is exercised through the running agent.
func TestApplyEdit(t *testing.T) {
	const (
		path  = "MEMORY.md"
		oldS  = "alpha"
		newS  = "beta"
		multi = "alpha and alpha"
	)

	cases := []struct {
		name       string
		content    string
		oldStr     string
		newStr     string
		replaceAll bool

		wantContent string
		wantCount   int
		wantErrSub  string // substring; empty == expect no error
	}{
		{
			name:        "single match replaces in place",
			content:     "x alpha y",
			oldStr:      oldS,
			newStr:      newS,
			wantContent: "x beta y",
			wantCount:   1,
		},
		{
			name:        "replace_all swaps every occurrence",
			content:     multi,
			oldStr:      oldS,
			newStr:      newS,
			replaceAll:  true,
			wantContent: "beta and beta",
			wantCount:   2,
		},
		{
			name:       "multi match without replace_all errors with count and hint",
			content:    multi,
			oldStr:     oldS,
			newStr:     newS,
			wantErrSub: "matches 2 locations",
		},
		{
			name:       "not found errors with path so the LLM knows which file to re-read",
			content:    "nothing here",
			oldStr:     oldS,
			newStr:     newS,
			wantErrSub: "not found in " + path,
		},
		{
			name:       "empty old_string rejected (use write_file instead)",
			content:    "anything",
			oldStr:     "",
			newStr:     newS,
			wantErrSub: "old_string is empty",
		},
		{
			name:       "no-op edit (old == new) rejected",
			content:    "x alpha y",
			oldStr:     oldS,
			newStr:     oldS,
			wantErrSub: "must differ",
		},
		{
			name:        "replace_all with single match still works",
			content:     "x alpha y",
			oldStr:      oldS,
			newStr:      newS,
			replaceAll:  true,
			wantContent: "x beta y",
			wantCount:   1,
		},
		{
			name:        "whitespace-sensitive match (indentation matters)",
			content:     "  alpha\n",
			oldStr:      "  alpha",
			newStr:      "  beta",
			wantContent: "  beta\n",
			wantCount:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, count, err := applyEdit(path, tc.content, tc.oldStr, tc.newStr, tc.replaceAll)

			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (content=%q)", tc.wantErrSub, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantContent {
				t.Errorf("content mismatch:\n  got:  %q\n  want: %q", got, tc.wantContent)
			}
			if count != tc.wantCount {
				t.Errorf("count mismatch: got %d, want %d", count, tc.wantCount)
			}
		})
	}
}

func TestManagedMemoryPathDetection(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"USER.md", true},
		{"MEMORY.md", true},
		{"User.md", true},
		{"memory.md", true},
		{`C:\agents\foo\USER.md`, true},
		{`C:/agents/foo/memory.md`, true},
		{`\\server\share\MEMORY.md`, true},
		{`/agents/foo/MEMORY.md`, true},
		{"notes/USER.md", false},
		{`notes\MEMORY.md`, false},
		{`notes\memory.md`, false},
		{"notes/MEMORY.md", false},
		{"SOUL.md", false},
	}
	for _, tc := range cases {
		if got := isManagedMemoryFilePath(tc.path); got != tc.want {
			t.Fatalf("isManagedMemoryFilePath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestFileToolsRefuseManagedMemoryFiles(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	assertFileToolsRefuseManagedMemoryFiles(t, r)
}

func TestFileToolsRefuseManagedMemoryFilesWhenManagedMemoryDisabled(t *testing.T) {
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetManagedMemoryConfig(memory.Config{Enabled: false})
	assertFileToolsRefuseManagedMemoryFiles(t, r)
}

func assertFileToolsRefuseManagedMemoryFiles(t *testing.T, r *Registry) {
	t.Helper()
	for _, name := range []string{"read_file", "write_file", "edit_file"} {
		var args string
		switch name {
		case "read_file":
			args = `{"path":"User.md"}`
		case "write_file":
			args = `{"path":"memory.md","content":"x"}`
		case "edit_file":
			args = `{"path":"USER.md","old_string":"x","new_string":"y"}`
		}
		got, err := r.Execute(context.Background(), name, args)
		if err != nil {
			t.Fatalf("%s returned error: %v", name, err)
		}
		if !strings.Contains(got, "managed memory resources") {
			t.Fatalf("%s result = %q", name, got)
		}
	}
}

func TestSandboxedFileToolsRefuseManagedMemoryFiles(t *testing.T) {
	ex := &fakeSandboxExecutor{}
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetExecutor(ex)

	assertFileToolsRefuseManagedMemoryFiles(t, r)
	if ex.readCalls != 0 || ex.writeCalls != 0 || ex.execCalls != 0 {
		t.Fatalf("managed-memory refusal should happen before sandbox calls, got read=%d write=%d exec=%d",
			ex.readCalls, ex.writeCalls, ex.execCalls)
	}
}

func TestSandboxedApplyPatchRefusesManagedMemoryFiles(t *testing.T) {
	ex := &fakeSandboxExecutor{}
	r := NewRegistry(t.TempDir(), t.TempDir())
	r.SetExecutor(ex)

	patch := `*** Begin Patch
*** Add File: memory.md
+fact
*** End Patch`
	raw, err := json.Marshal(map[string]string{"input": patch})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	got, err := r.Execute(context.Background(), "apply_patch", string(raw))
	if err == nil {
		t.Fatalf("expected apply_patch refusal, got result %q", got)
	}
	if !strings.Contains(got+err.Error(), "managed memory resources") {
		t.Fatalf("expected managed-memory refusal, got result %q err %v", got, err)
	}
	if ex.readCalls != 0 || ex.writeCalls != 0 || ex.execCalls != 0 {
		t.Fatalf("managed-memory refusal should happen before sandbox calls, got read=%d write=%d exec=%d",
			ex.readCalls, ex.writeCalls, ex.execCalls)
	}
}

type fakeSandboxExecutor struct {
	readCalls  int
	writeCalls int
	execCalls  int
}

func (f *fakeSandboxExecutor) Exec(ctx context.Context, command string, timeout time.Duration) (string, error) {
	f.execCalls++
	return "exec", nil
}

func (f *fakeSandboxExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	f.readCalls++
	return "old\n", nil
}

func (f *fakeSandboxExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	f.writeCalls++
	return "written", nil
}

func (f *fakeSandboxExecutor) ListDir(ctx context.Context, path string) (string, error) {
	return "", nil
}

func (f *fakeSandboxExecutor) Backend() string { return "fake" }

func (f *fakeSandboxExecutor) Close() error { return nil }
