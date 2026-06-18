package agent

import (
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/bus"
	"github.com/qs3c/bkclaw/internal/config"
	"github.com/qs3c/bkclaw/internal/provider"
	"github.com/qs3c/bkclaw/internal/session"
)

func TestSlashCompactUsesManualFocus(t *testing.T) {
	sessions := session.NewManager(t.TempDir())
	msg := bus.InboundMessage{
		Channel: "web",
		ChatID:  "chat",
		UserID:  "owner",
		Text:    "/compact preserve filesystem decisions",
	}
	sess := sessions.Get(msg.Channel, msg.AccountID, msg.ChatID, msg.ProjectID)
	for i := 0; i < 5; i++ {
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

	result := a.handleSlashCommand(msg)
	if !result.handled {
		t.Fatal("/compact should be handled")
	}
	if !strings.Contains(result.reply, "Compacted checkpoint") {
		t.Fatalf("unexpected compact reply: %s", result.reply)
	}
	if !strings.Contains(f.gotSummaryRequest, "Manual compaction focus:\npreserve filesystem decisions") {
		t.Fatalf("manual focus missing from summary request: %s", f.gotSummaryRequest)
	}
	if got := len(sess.GetMessages()); got >= 10 {
		t.Fatalf("session messages were not compacted, got %d", got)
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
