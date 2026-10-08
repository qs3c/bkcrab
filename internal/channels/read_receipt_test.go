package channels

import (
	"context"
	"testing"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
)

type receiptTestChannel struct {
	*OpenIM
	calls   int
	bounded bool
}

func (c *receiptTestChannel) MarkRead(ctx context.Context, _ bus.InboundMessage) error {
	c.calls++
	deadline, ok := ctx.Deadline()
	c.bounded = ok && time.Until(deadline) <= 5*time.Second
	return nil
}
func TestManagerReadReceiptRouting(t *testing.T) {
	o, _ := NewOpenIM(openIMTestConfig(), nil, nil)
	ch := &receiptTestChannel{OpenIM: o}
	m := NewManager(bus.New())
	m.Register(ch)
	msg := bus.InboundMessage{Channel: "openim", AccountID: o.AccountID(), MessageID: "one"}
	m.MarkRead(context.Background(), msg)
	if ch.calls != 1 || !ch.bounded {
		t.Fatal("receipt not dispatched with deadline")
	}
	msg.AccountID = "other"
	m.MarkRead(context.Background(), msg)
	msg.AccountID = o.AccountID()
	msg.Source = bus.SourceCron
	m.MarkRead(context.Background(), msg)
	if ch.calls != 1 {
		t.Fatal("wrong account or synthetic message dispatched")
	}
}

type waitingLease struct {
	NopLeaser
	attempted chan struct{}
}

func (l waitingLease) Acquire(context.Context, string, string, string, time.Duration) (bool, error) {
	close(l.attempted)
	return false, nil
}

func TestOpenIMPresenceStopWhileWaitingForLease(t *testing.T) {
	o, _ := NewOpenIM(openIMTestConfig(), bus.New(), &openIMTestInbox{})
	attempted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWithLease(context.Background(), o, waitingLease{attempted: attempted}, "holder")
	}()
	select {
	case <-attempted:
	case <-time.After(time.Second):
		t.Fatal("no lease attempt")
	}
	o.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stopped binding left a lease waiter")
	}
}
