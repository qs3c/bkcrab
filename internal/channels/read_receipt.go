package channels

import (
	"context"
	"log/slog"
	"time"

	"github.com/qs3c/bkcrab/internal/bus"
)

// MarkRead acknowledges intake by a channel that supports receipts. It is best
// effort and bounded; an unavailable IM service must not abort an agent turn.
func (m *Manager) MarkRead(ctx context.Context, msg bus.InboundMessage) {
	if m == nil || msg.Source != bus.SourceUser || msg.IsBotMessage || msg.MessageID == "" {
		return
	}
	ch, ok := m.Get(msg.Channel, msg.AccountID).(interface {
		MarkRead(context.Context, bus.InboundMessage) error
	})
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ch.MarkRead(ctx, msg); err != nil {
		// Capability implementations may wrap URLs. Keep raw errors out of logs.
		slog.Warn("channel read receipt failed", "channel", msg.Channel, "account", msg.AccountID)
	}
}
