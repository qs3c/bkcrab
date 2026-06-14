//go:build !windows

package gateway

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyReloadSignal 要求运行时将 SIGHUP 传递到 ch 上。每当 ch 触发时，
// 网关热重载代理，这允许进程外的调用者（CLI、操作员的 `kill -HUP $PID`）
// 触发重新加载而无需重启网关。
func notifyReloadSignal(ch chan os.Signal) {
	signal.Notify(ch, syscall.SIGHUP)
}
