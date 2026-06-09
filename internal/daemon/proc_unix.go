//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"syscall"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func isCleanShutdown(exitErr *exec.ExitError) bool {
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if status.Signal() == syscall.SIGTERM || status.Signal() == syscall.SIGINT {
			return true
		}
	}
	return false
}

func signalProcess(proc *os.Process, sig string) error {
	switch sig {
	case "TERM":
		return proc.Signal(syscall.SIGTERM)
	case "KILL":
		return proc.Signal(syscall.SIGKILL)
	case "CHECK":
		return proc.Signal(syscall.Signal(0))
	case "RELOAD":
		return proc.Signal(syscall.SIGHUP)
	}
	return nil
}
