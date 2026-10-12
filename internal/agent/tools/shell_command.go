package tools

import (
	"context"
	"os/exec"
	"time"
)

// shellWaitDelay bounds how long a synchronous exec tool call waits for the
// command's stdout/stderr pipes to close after the shell has exited or the
// call's context was canceled.
//
// os/exec's documented behaviour is to read a non-*os.File Stdout/Stderr
// through an internal pipe until EOF. A descendant that inherited that pipe
// (e.g. a `grep ... | sort | head` pipeline orphaned when its `sh` parent is
// killed, or a backgrounded daemon that outlives the shell) keeps the write
// end open, so the copy goroutine never sees EOF and Cmd.Wait blocks
// forever. WaitDelay forces the pipes closed after the delay and makes Wait
// return exec.ErrWaitDelay, so the tool call always returns to the agent
// loop. Partial output written before the delay is preserved.
const shellWaitDelay = 3 * time.Second

// runShellCommand runs `sh -c command` for the synchronous exec/host_exec
// tools with three hard guarantees:
//
//  1. The shell starts its own process group (setProcessGroup), so
//     descendants — pipeline stages, job children — are reachable by a
//     single group signal.
//  2. Context cancellation (the per-call timeout) overrides
//     exec.CommandContext's default single-process kill and instead
//     SIGKILLs the whole group (killProcessGroup). Without this, killing the
//     shell leaves `grep`/`sort`/`head` reparented to PID 1, still holding
//     the output pipe.
//  3. WaitDelay bounds the pipe drain, so even a descendant that escaped the
//     group (or a backgrounded process that outlives a normally-exited
//     shell) cannot stall the tool call until the harness-level agent
//     timeout fires.
//
// dir is the working directory ("" means inherit the caller's); env is the
// fully-scrubbed subprocess environment; nil env falls back to Go's default
// inheritance and should be avoided on the host path.
func runShellCommand(ctx context.Context, command, dir string, env []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	setProcessGroup(cmd)
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killProcessGroup(cmd.Process.Pid)
	}
	cmd.WaitDelay = shellWaitDelay
	return cmd.CombinedOutput()
}
