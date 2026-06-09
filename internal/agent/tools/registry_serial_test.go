package tools

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRegisterSerial_BlocksConcurrentSelfCalls fans out 5 goroutines into
// one serial-registered tool and asserts only one is inside the fn at any
// moment. This is the exact failure mode the delegate_task fan-out hit:
// multiple sub-agent invocations sharing a single browser daemon would
// step on each other's state. The mutex wrapper has to serialize them
// transparently — callers shouldn't need to coordinate.
func TestRegisterSerial_BlocksConcurrentSelfCalls(t *testing.T) {
	r := NewRegistry("", "")

	var inFlight atomic.Int32
	var peak atomic.Int32
	r.RegisterSerial("slow_tool", "test", nil, func(ctx context.Context, args json.RawMessage) (string, error) {
		cur := inFlight.Add(1)
		// Record the highest "concurrent in fn" we ever saw — must
		// stay at 1 for serial to be doing its job.
		for {
			p := peak.Load()
			if cur <= p || peak.CompareAndSwap(p, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		inFlight.Add(-1)
		return "ok", nil
	})

	fn := r.GetFunc("slow_tool")
	if fn == nil {
		t.Fatal("tool not registered")
	}

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = fn(context.Background(), nil)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got != 1 {
		t.Fatalf("serial guarantee violated: peak concurrent in fn = %d, want 1", got)
	}
}

// TestRegisterSerial_DoesNotBlockOtherTools confirms the mutex is
// per-tool, not global — a slow serial tool must not pin a fast
// unrelated tool. Otherwise an in-flight delegate_task would freeze
// every other tool the parent agent wants to run alongside.
func TestRegisterSerial_DoesNotBlockOtherTools(t *testing.T) {
	r := NewRegistry("", "")

	released := make(chan struct{})
	r.RegisterSerial("slow_serial", "test", nil, func(ctx context.Context, args json.RawMessage) (string, error) {
		<-released
		return "slow_done", nil
	})
	r.Register("fast_other", "test", nil, func(ctx context.Context, args json.RawMessage) (string, error) {
		return "fast_done", nil
	})

	// Start the slow serial call; it'll block until we release it.
	slowDone := make(chan string)
	go func() {
		out, _ := r.GetFunc("slow_serial")(context.Background(), nil)
		slowDone <- out
	}()

	// Other tool must complete immediately — the serial mutex is per
	// tool, not a global gate.
	fastOut, _ := r.GetFunc("fast_other")(context.Background(), nil)
	if fastOut != "fast_done" {
		t.Fatalf("other tool didn't run: got %q", fastOut)
	}

	close(released)
	if got := <-slowDone; got != "slow_done" {
		t.Fatalf("slow tool didn't run: got %q", got)
	}
}
