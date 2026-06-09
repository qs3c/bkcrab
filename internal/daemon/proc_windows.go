//go:build windows

package daemon

import (
	"os"
	"os/exec"
)

func setSysProcAttr(cmd *exec.Cmd) {}

func isCleanShutdown(exitErr *exec.ExitError) bool {
	return false
}

func signalProcess(proc *os.Process, sig string) error {
	switch sig {
	case "TERM", "KILL":
		return proc.Kill()
	case "CHECK":
		// On Windows, just try to open the process
		return nil
	case "RELOAD":
		// SIGHUP is not deliverable on Windows. Callers fall back to a
		// hint asking the operator to restart the gateway.
		return errReloadUnsupported
	}
	return nil
}

// errReloadUnsupported is returned by signalProcess on Windows so the
// CLI can detect "no graceful reload available here" and degrade.
var errReloadUnsupported = winReloadErr("reload via signal is not supported on Windows")

type winReloadErr string

func (e winReloadErr) Error() string { return string(e) }
