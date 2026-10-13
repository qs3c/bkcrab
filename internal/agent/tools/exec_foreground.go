package tools

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"time"
)

// Bound pipe draining after cancellation OR shell exit. A descendant may still
// own stdout/stderr even after the shell has been reaped.
const foregroundOutputWait = 2 * time.Second

func runForegroundShell(ctx context.Context, command, dir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	cmd.WaitDelay = foregroundOutputWait
	if runtime.GOOS != "windows" {
		setProcessGroup(cmd)
		cmd.Cancel = func() error {
			return killProcessGroup(cmd.Process.Pid)
		}
	}

	out, err := cmd.CombinedOutput()
	if err != nil && cmd.Process != nil && runtime.GOOS != "windows" {
		// WaitDelay closes pipes but only kills the direct child. Also clean up
		// the group when the shell exited before cancellation (including a
		// nonzero exit, which can mask ErrWaitDelay). Do not signal on Start failure.
		err = errors.Join(err, killProcessGroup(cmd.Process.Pid))
	}
	if err != nil && ctx.Err() != nil {
		// Preserve both the cancellation reason and the underlying exit status.
		err = errors.Join(ctx.Err(), err)
	}
	return out, err
}
