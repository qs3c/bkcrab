//go:build unix

package tools

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup configures the command to spawn into a NEW process
// group (Setpgid=true with Pgid=0 makes the child the group leader).
// Must be set before cmd.Start. Together with killProcessGroup this
// guarantees `kill_shell` reaches every descendant the command spawned
// — without it, killing `sh -c "npm run dev"` leaves the node process
// orphaned and still bound to the dev server's port.
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup sends SIGKILL to the entire process group whose
// leader's PID is groupLeaderPid. The negative-pid trick (kill(2) with
// -pid) is the POSIX idiom for group signals. ESRCH ("no such process")
// is mapped to nil — that just means the group already terminated,
// which is the same end state we wanted.
func killProcessGroup(groupLeaderPid int) error {
	if groupLeaderPid <= 0 {
		return nil
	}
	if err := syscall.Kill(-groupLeaderPid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}
