package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/workspace"
)

func TestHostExecAndFileToolsShareIsolatedSessionWorkspaces(t *testing.T) {
	for _, useStore := range []bool{false, true} {
		t.Run(fmt.Sprint(useStore), func(t *testing.T) {
			root := t.TempDir()
			parent := NewRegistry(t.TempDir(), filepath.Join(root, "agent"))
			defer parent.Close()
			if useStore {
				parent.SetWorkspaceStore(workspace.NewMetered(workspace.NewLocalFS(root), func(context.Context, string, int64) {}), "agent")
			}
			var wg sync.WaitGroup
			for i := 0; i < 24; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					rt := parent.ForTurn()
					rt.SetSessionID(fmt.Sprintf("s-%d", i))
					if i%2 == 0 {
						rt.SetProjectID("shared-project")
					}
					plan := fmt.Sprintf("PLAN-%d", i)
					args, _ := json.Marshal(map[string]string{"path": "todo.md", "content": plan})
					if _, err := rt.Execute(context.Background(), "write_file", string(args)); err != nil {
						t.Error(err)
						return
					}
					out, err := rt.Execute(context.Background(), "exec", `{"command":"cat todo.md; printf ' done' >> todo.md"}`)
					if err != nil || out != plan {
						t.Errorf("session %d exec = %q, %v", i, out, err)
						return
					}
					out, err = rt.Execute(context.Background(), "read_file", `{"path":"todo.md"}`)
					if err != nil || !strings.Contains(out, plan+" done") {
						t.Errorf("session %d read = %q, %v", i, out, err)
					}
				}(i)
			}
			wg.Wait()
		})
	}
}

func TestBackgroundExecUsesSessionDirectory(t *testing.T) {
	parent := NewRegistry(t.TempDir(), t.TempDir())
	defer parent.Close()
	rt := parent.ForTurn()
	rt.SetProjectID("project")
	rt.SetSessionID("session")
	_, err := rt.Execute(context.Background(), "exec", `{"command":"printf BACKGROUND > todo.md","run_in_background":true}`)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out, err := rt.Execute(context.Background(), "read_file", `{"path":"todo.md"}`)
		if err == nil && strings.Contains(out, "BACKGROUND") {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("background shell did not write the session's todo.md")
}
