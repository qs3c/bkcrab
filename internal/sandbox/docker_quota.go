package sandbox

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// ConfigureQuota is called at startup. The resolver uses authoritative agent
// ownership, not an owner supplied by a shell command or model tool arguments.
func (p *DockerExecutorPool) ConfigureQuota(l Limits, owner func(context.Context, string) (string, error)) {
	p.limits = l
	p.owner = owner
}
func (p *DockerExecutorPool) prepareQuota(ctx context.Context, agent string) error {
	if p.limits.QuotaImage == "" {
		return nil
	}
	if p.owner == nil {
		return fmt.Errorf("workspace quota requires an agent owner resolver")
	}
	owner, err := p.owner(ctx, agent)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--privileged",
		"--memory", "256m", "--cpus", "0.5", "--pids-limit", "64",
		"-v", filepath.Join(p.workspaceRoot, "workspaces")+":/quota",
		p.limits.QuotaImage, owner, agent, strconv.FormatInt(p.limits.WorkspaceBytes, 10), strconv.Itoa(p.limits.WorkspaceFiles))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("workspace hard quota provisioning failed: %s: %w", out, err)
	}
	return nil
}
