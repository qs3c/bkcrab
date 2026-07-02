package agent

import (
	"context"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/provider"
)

func (a *Agent) runBeforeModelCallHooks(ctx context.Context, messages []provider.Message, msg bus.InboundMessage) ([]provider.Message, *HookContext) {
	hc := &HookContext{
		AgentName: a.name,
		Point:     BeforeModelCall,
		Messages:  messages,
		Channel:   msg.Channel,
		AccountID: msg.AccountID,
		ChatID:    msg.ChatID,
		UserID:    a.ownerUserID,
	}
	if a.hooks != nil {
		a.hooks.Run(ctx, hc)
	}
	return hc.Messages, hc
}
