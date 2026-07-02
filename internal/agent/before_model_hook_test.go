package agent

import (
	"context"
	"reflect"
	"testing"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/provider"
)

func TestRunBeforeModelCallHooksUsesMutatedMessages(t *testing.T) {
	original := []provider.Message{{Role: "user", Content: "hello"}}
	injected := []provider.Message{
		{Role: "system", Content: "remembered context"},
		{Role: "user", Content: "hello"},
	}

	a := &Agent{
		name:        "agent-1",
		ownerUserID: "owner-1",
		hooks:       NewHookRegistry(),
	}
	a.hooks.Register(BeforeModelCall, func(ctx context.Context, hc *HookContext) {
		hc.Messages = injected
	})

	got, hc := a.runBeforeModelCallHooks(context.Background(), original, bus.InboundMessage{
		Channel:   "web",
		AccountID: "acct-1",
		ChatID:    "chat-1",
	})

	if !reflect.DeepEqual(got, injected) {
		t.Fatalf("messages = %#v, want %#v", got, injected)
	}
	if hc.UserID != "owner-1" {
		t.Fatalf("hook user id = %q, want owner-1", hc.UserID)
	}
	if hc.ChatID != "chat-1" {
		t.Fatalf("hook chat id = %q, want chat-1", hc.ChatID)
	}
}
