//go:build !windows

package daemon

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// TestSignalReloadDeliversSIGHUP confirms that SignalReload(self) lands
// as a real SIGHUP on the test process. This is the contract the CLI's
// hot-reload path depends on — without SIGHUP delivery, `agents` writes
// would silently leave the running gateway with stale caches.
func TestSignalReloadDeliversSIGHUP(t *testing.T) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)

	if err := SignalReload(os.Getpid()); err != nil {
		t.Fatalf("SignalReload: %v", err)
	}

	select {
	case sig := <-ch:
		if sig != syscall.SIGHUP {
			t.Fatalf("expected SIGHUP, got %v", sig)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for SIGHUP")
	}
}
