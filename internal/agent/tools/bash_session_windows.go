//go:build windows

package tools

import "os/exec"

// Windows has no Setpgid analogue exposed via syscall.SysProcAttr — job
// objects exist but require non-trivial wiring. FastClaw's exec path is
// already Unix-only in practice (uses `sh -c`), so a no-op here keeps
// cross-compilation green without pretending to support Windows.
//
// Practical consequence: on Windows the default cmd.Cancel (Kill on the
// direct child only) applies, and grandchildren can survive a
// kill_shell. Acceptable for v1 — the same gap exists in the
// synchronous exec path.
func setProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(groupLeaderPid int) error { return nil }
