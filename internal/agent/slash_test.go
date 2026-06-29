package agent

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/qs3c/bkcrab/internal/bus"
	"github.com/qs3c/bkcrab/internal/config"
	"github.com/qs3c/bkcrab/internal/provider"
	"github.com/qs3c/bkcrab/internal/session"
)

type ctxKey string

func TestSlashCompactUsesManualFocus(t *testing.T) {
	sessions := session.NewManager(t.TempDir())
	msg := bus.InboundMessage{
		Channel: "web",
		ChatID:  "chat",
		UserID:  "owner",
		Text:    "/compact preserve filesystem decisions",
	}
	sess := sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	for i := 0; i < 12; i++ {
		sess.Append(provider.Message{Role: "user", Content: "user turn", Origin: provider.OriginUser})
		sess.Append(provider.Message{Role: "assistant", Content: "assistant turn", Origin: provider.OriginUser})
	}

	f := &fakeSummarizer{}
	a := &Agent{
		provider:      f,
		sessions:      sessions,
		model:         "fake-model",
		maxTokens:     1000,
		contextWindow: 1000000,
		homePath:      t.TempDir(),
		ownerUserID:   "owner",
	}

	const sentinel ctxKey = "compact-ctx-marker"
	ctx := context.WithValue(context.Background(), sentinel, "present")

	result := a.handleSlashCommand(ctx, msg)
	if !result.handled {
		t.Fatal("/compact should be handled")
	}
	if !strings.Contains(result.reply, "Compacted checkpoint") {
		t.Fatalf("unexpected compact reply: %s", result.reply)
	}
	if !strings.Contains(f.gotSummaryRequest, "Manual compaction focus:\npreserve filesystem decisions") {
		t.Fatalf("manual focus missing from summary request: %s", f.gotSummaryRequest)
	}
	// The turn ctx must reach Provider.Chat: a nil ctx fails every OpenAI-compatible
	// summary attempt and silently degrades manual /compact to the crude fallback.
	if f.gotCtx == nil {
		t.Fatal("summary provider received a nil ctx; manual /compact would degrade to the deterministic fallback for OpenAI-compatible providers")
	}
	if got, _ := f.gotCtx.Value(sentinel).(string); got != "present" {
		t.Fatalf("turn ctx did not propagate to summary provider: Value(%q) = %q, want %q", sentinel, got, "present")
	}
	if got := len(sess.GetMessages()); got >= 24 {
		t.Fatalf("session messages were not compacted, got %d", got)
	}
}

func TestSlashCompactEmitsProgressEvents(t *testing.T) {
	sessions := session.NewManager(t.TempDir())
	msg := bus.InboundMessage{
		Channel: "web",
		ChatID:  "chat",
		UserID:  "owner",
		Text:    "/compact",
	}
	sess := sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	for i := 0; i < 12; i++ {
		sess.Append(provider.Message{Role: "user", Content: "user turn", Origin: provider.OriginUser})
		sess.Append(provider.Message{Role: "assistant", Content: "assistant turn", Origin: provider.OriginUser})
	}

	a := &Agent{
		provider:      &fakeSummarizer{},
		sessions:      sessions,
		model:         "fake-model",
		maxTokens:     1000,
		contextWindow: 1000000,
		homePath:      t.TempDir(),
		ownerUserID:   "owner",
	}

	events := make(chan ChatEvent, 4)
	ctx := ContextWithChatEvents(context.Background(), events)

	result := a.handleSlashCommand(ctx, msg)
	if !result.handled {
		t.Fatal("/compact should be handled")
	}

	var got []bool
	for {
		select {
		case evt := <-events:
			if evt.Type == "compaction" {
				active, ok := evt.Data["active"].(bool)
				if !ok {
					t.Fatalf("compaction event active = %#v, want bool", evt.Data["active"])
				}
				got = append(got, active)
			}
		default:
			want := []bool{true, false}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("compaction active events = %v, want %v", got, want)
			}
			return
		}
	}
}

func TestSlashModelRefreshesContextWindow(t *testing.T) {
	a := &Agent{
		model:         "old-model",
		maxTokens:     4096,
		contextWindow: 32000,
		providerConfigs: map[string]config.ProviderConfig{
			"openai": {
				Models: []config.ModelEntry{
					{ID: "large-model", ContextWindow: 200000},
				},
			},
		},
	}

	result := a.slashModel(bus.InboundMessage{}, "openai/large-model")
	if !result.handled {
		t.Fatal("/model should be handled")
	}
	if a.model != "openai/large-model" {
		t.Fatalf("model = %q", a.model)
	}
	if a.contextWindow != 200000 {
		t.Fatalf("context window = %d, want 200000", a.contextWindow)
	}
}
