//go:build unix

package tools

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup 配置命令以生成新进程
// 组（Setpgid=true 且 Pgid=0 使子组成为组长）。
// 必须在 cmd.Start 之前设置。与killProcessGroup一起使用
// 保证“kill_shell”到达命令生成的每个后代
// — 没有它，杀死 `sh -c "npm run dev"` 会留下节点进程
// 孤立并且仍然绑定到开发服务器的端口。
func setProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillProcessGroup 向其所属的整个进程组发送 SIGKILL
// 领导者的 PID 是 groupLeaderPid。负 pid 技巧 (kill(2) 与
// -pid) 是组信号的 POSIX 习惯用法。 ESRCH（“没有这样的过程”）
// 映射到 nil — 这仅仅意味着该组已经终止，
// 这与我们想要的最终状态相同。
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
