package sandbox

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

func ownerLabel(home string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(home)))[:24] }

func (s *DockerSandbox) ensureRunning(ctx context.Context) error {
	s.mu.Lock()
	id := s.containerID
	s.mu.Unlock()
	if id == "" {
		return s.Create()
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Running}}", id).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "No such object") || strings.Contains(string(out), "No such container") {
			s.mu.Lock()
			s.containerID = ""
			s.mu.Unlock()
			return s.Create()
		}
		return fmt.Errorf("inspect sandbox: %s: %w", out, err)
	}
	if strings.TrimSpace(string(out)) == "true" {
		return nil
	}
	if out, err := exec.CommandContext(ctx, "docker", "start", id).CombinedOutput(); err != nil {
		return fmt.Errorf("restart sandbox: %s: %w", out, err)
	}
	return nil
}

// Recover removes only sandboxes created by this deployment's pool. A crashed
// gateway cannot resume the old in-memory turn; leaving its containers running
// would bypass the new pool's capacity accounting. Workspace mounts survive.
func (p *DockerExecutorPool) Recover(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	label := "bkcrab.pool=" + ownerLabel(p.workspaceRoot)
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "label="+label).CombinedOutput()
	if err != nil {
		return fmt.Errorf("list stale sandboxes: %s: %w", out, err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"rm", "-f"}, ids...)
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("remove stale sandboxes: %s: %w", out, err)
	}
	return nil
}
