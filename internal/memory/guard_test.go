package memory

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/jev"
)

func TestGuardAuditDoesNotLogMemory(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	guard := NewJevGuard(&guardDecider{p: .4}, "shadow")
	guard.Review(context.Background(), TargetMemory, []string{"private-project-fact-12345"})
	logged := output.String()
	if strings.Contains(logged, "private-project-fact-12345") || !strings.Contains(logged, `"probabilities"`) || !strings.Contains(logged, `"action":"review"`) || !strings.Contains(logged, `"content_hash"`) {
		t.Fatalf("unexpected audit fields: %s", logged)
	}
}

type guardFunc func(context.Context, Target, []string) GuardResult

func (f guardFunc) Review(c context.Context, t Target, e []string) GuardResult { return f(c, t, e) }

type guardDecider struct {
	p     float64
	err   error
	calls int
}

func (f *guardDecider) Decide(c context.Context, s any, q map[string]jev.Question) (jev.Result, error) {
	f.calls++
	r := jev.Result{Probabilities: map[string]float64{}, Model: "m"}
	for k := range q {
		r.Probabilities[k] = f.p
	}
	return r, f.err
}
func TestGuardShadowAndEnforce(t *testing.T) {
	for _, mode := range []string{"shadow", "enforce"} {
		for _, fail := range []bool{false, true} {
			t.Run(mode+map[bool]string{true: "-error"}[fail], func(t *testing.T) {
				d := &guardDecider{p: .9}
				if fail {
					d.err = errors.New("provider down")
				}
				m := NewManager(Options{Root: t.TempDir(), Config: DefaultConfig(), Guard: NewJevGuard(d, mode)})
				r := m.Apply(context.Background(), TargetMemory, []Operation{{Action: ActionAdd, Content: "unclassified text"}})
				if r.Success != (mode == "shadow") {
					t.Fatal(r)
				}
				if d.calls != 1 {
					t.Fatal("missing review")
				}
			})
		}
	}
}
func TestGuardComposedSnapshotAndConcurrentWrite(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "MEMORY.md")
	m := NewManager(Options{Root: root, Config: DefaultConfig()})
	m.Apply(context.Background(), TargetMemory, []Operation{{Action: ActionAdd, Content: "existing"}})
	m.guard = guardFunc(func(_ context.Context, _ Target, entries []string) GuardResult {
		if len(entries) != 3 {
			t.Fatalf("not full proposed content %v", entries)
		}
		other := NewManager(Options{Root: root, Config: DefaultConfig()})
		if r := other.Apply(context.Background(), TargetMemory, []Operation{{Action: ActionAdd, Content: "concurrent"}}); !r.Success {
			t.Fatal("review held mutation lock", r)
		}
		return GuardResult{Enforced: true, Allowed: true}
	})
	r := m.Apply(context.Background(), TargetMemory, []Operation{{Action: ActionAdd, Content: "part one"}, {Action: ActionAdd, Content: "part two"}})
	if r.Success {
		t.Fatal("accepted stale review")
	}
	data, _ := os.ReadFile(path)
	entries, _ := parseEntries(TargetMemory, data)
	if len(entries) != 2 || entries[1] != "concurrent" {
		t.Fatalf("lost concurrent data: %s", data)
	}
}
func TestGuardLocalRejectionAndRemovalNoNetwork(t *testing.T) {
	d := &guardDecider{p: .1}
	m := NewManager(Options{Root: t.TempDir(), Config: DefaultConfig(), Guard: NewJevGuard(d, "enforce")})
	if r := m.Apply(context.Background(), TargetMemory, []Operation{{Action: ActionAdd, Content: "-----BEGIN RSA PRIVATE KEY-----"}}); r.Success {
		t.Fatal("accepted credential")
	}
	if d.calls != 0 {
		t.Fatal("credential sent externally")
	}
	if r := m.Apply(context.Background(), TargetMemory, []Operation{{Action: ActionAdd, Content: "用户喜欢中文"}}); !r.Success {
		t.Fatal(r)
	}
	d.err = errors.New("offline")
	if r := m.Apply(context.Background(), TargetMemory, []Operation{{Action: ActionRemove, OldText: "用户喜欢中文"}}); !r.Success {
		t.Fatal("removal blocked", r)
	}
	if d.calls != 1 {
		t.Fatal("removal called provider")
	}
}
