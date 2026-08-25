package dialogue

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTurnUnmarshalSupportsLegacyAndRoleAwareHistory(t *testing.T) {
	var history []Turn
	if err := json.Unmarshal([]byte(`[
		"legacy user question",
		{"role":"agent","content":"agent reply"},
		{"role":"user","content":"follow-up"}
	]`), &history); err != nil {
		t.Fatal(err)
	}
	want := []Turn{
		{Role: RoleUser, Content: "legacy user question"},
		{Role: RoleAssistant, Content: "agent reply"},
		{Role: RoleUser, Content: "follow-up"},
	}
	if len(history) != len(want) {
		t.Fatalf("history = %+v", history)
	}
	for index := range want {
		if history[index] != want[index] {
			t.Fatalf("history[%d] = %+v, want %+v", index, history[index], want[index])
		}
	}
	encoded, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"legacy user question"`) && !strings.Contains(string(encoded), `"role":"user"`) {
		t.Fatalf("new history was not role-aware: %s", encoded)
	}
}

func TestNormalizeKeepsRecentTurnsAndRuneBudget(t *testing.T) {
	history := []Turn{
		NewTurn("user", "old"),
		NewTurn("assistant", "reply"),
		NewTurn("user", "newest"),
	}
	got := Normalize(history, 2, 100)
	if len(got) != 2 || got[0].Role != RoleAssistant || got[1].Content != "newest" {
		t.Fatalf("count-normalized history = %+v", got)
	}
	got = Normalize([]Turn{NewTurn("user", strings.Repeat("旧", 8)), NewTurn("assistant", "最新")}, 20, 4)
	if len(got) != 1 || got[0].Role != RoleAssistant || got[0].Content != "最新" {
		t.Fatalf("rune-normalized history = %+v", got)
	}
}
