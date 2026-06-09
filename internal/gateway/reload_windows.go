//go:build windows

package gateway

import "os"

// notifyReloadSignal is a no-op on Windows: SIGHUP is not delivered. The
// CLI falls back to printing a "restart the gateway" hint after a write.
func notifyReloadSignal(_ chan os.Signal) {}
