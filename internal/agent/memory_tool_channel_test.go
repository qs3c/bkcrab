package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/qs3c/bkclaw/internal/memory"
	"github.com/qs3c/bkclaw/internal/store"
)

// TestAutoPersistMemoryPassesExtractionTool asserts the cadence extraction now offers
// the persist_memory tool to the model (the structured channel) instead of tools=nil.
func TestAutoPersistMemoryPassesExtractionTool(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	prov := &fakeExtractProvider{resp: `{"memory_ops":[],"user_ops":[]}`}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "hi"}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "m", groups, 0, 0); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}
	if len(prov.gotTools) != 1 || prov.gotTools[0].Function.Name != extractMemoryToolName {
		t.Fatalf("expected the %q tool to be offered, got %+v", extractMemoryToolName, prov.gotTools)
	}
}

// TestAutoPersistMemoryAppliesToolCallOps asserts that when the model answers through
// the tool channel (ToolCalls, not prose), the ops are read from the tool arguments and
// applied — no free-text JSON parsing involved.
func TestAutoPersistMemoryAppliesToolCallOps(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	prov := &fakeExtractProvider{
		toolArgs: `{"memory_ops":[{"action":"add","content":"fact via tool"}],"user_ops":[{"action":"add","content":"name is Bo"}]}`,
	}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "remember this"}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "m", groups, 0, 0); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}
	if strings.Join(mgr.List(context.Background(), memory.TargetMemory).Entries, "|") != "fact via tool" {
		t.Fatalf("memory ops from tool channel not applied")
	}
	if strings.Join(mgr.List(context.Background(), memory.TargetUser).Entries, "|") != "name is Bo" {
		t.Fatalf("user ops from tool channel not applied")
	}
}

// TestAutoPersistMemoryFallsBackToProseJSON asserts the free-text path still works when
// the model ignores the tool and replies with JSON in the message body (weak backends /
// cheap models that don't reliably call tools).
func TestAutoPersistMemoryFallsBackToProseJSON(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	prov := &fakeExtractProvider{resp: "```json\n{\"memory_ops\":[{\"action\":\"add\",\"content\":\"prose fallback fact\"}],\"user_ops\":[]}\n```"}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: "hi"}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "m", groups, 0, 0); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}
	if strings.Join(mgr.List(context.Background(), memory.TargetMemory).Entries, "|") != "prose fallback fact" {
		t.Fatalf("prose JSON fallback not applied")
	}
}
