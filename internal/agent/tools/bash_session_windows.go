//go:build windows

package tools

import "os/exec"

// Windows 没有通过 syscall.SysProcAttr 公开的 Setpgid 类似物 — 作业
// 对象存在但需要不平凡的接线。 BkCrab的执行路径是
// 实际上已经仅限于 Unix（使用 `sh -c`），因此这里的无操作保留
// 绿色交叉编译，无需假装支持Windows。
//
// 实际结果：在 Windows 上，默认 cmd.Cancel（在
// 仅直系子女）适用，并且孙辈可以存活
// 杀壳。 v1 可以接受。同步 exec 路径共享无操作
// setProcessGroup/killProcessGroup，但它另外设置
// Cmd.WaitDelay，因此保留的管道最多将调用限制为
// shellWaitDelay 而不是永远挂起。
func setProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(groupLeaderPid int) error { return nil }
