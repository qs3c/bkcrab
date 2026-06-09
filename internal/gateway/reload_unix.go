//go:build !windows

package gateway

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyReloadSignal asks the runtime to deliver SIGHUP onto ch. The
// gateway hot-reloads agents whenever ch fires, which lets out-of-
// process callers (the CLI, an operator's `kill -HUP $PID`) trigger a
// reload without restarting the gateway.
func notifyReloadSignal(ch chan os.Signal) {
	signal.Notify(ch, syscall.SIGHUP)
}
