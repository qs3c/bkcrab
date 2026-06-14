//go:build windows

package gateway

import "os"

// notifyReloadSignal 在 Windows 上是空操作：SIGHUP 不会被传递。
// CLI 回退到在写入后打印"重新启动网关"的提示。
func notifyReloadSignal(_ chan os.Signal) {}
