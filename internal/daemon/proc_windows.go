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
		// 在 Windows 上，仅尝试打开进程
		return nil
	case "RELOAD":
		// SIGHUP 在 Windows 上不可发送。调用者回退到
		// 提示操作员重新启动网关。
		return errReloadUnsupported
	}
	return nil
}

// errReloadUnsupported 由 signalProcess 在 Windows 上返回，以便
// CLI 可以检测到"此处不支持优雅重新加载"并降级处理。
var errReloadUnsupported = winReloadErr("reload via signal is not supported on Windows")

type winReloadErr string

func (e winReloadErr) Error() string { return string(e) }
