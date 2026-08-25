package eval

import (
	"testing"

	"github.com/qs3c/bkcrab/internal/rag/dialogue"
)

func TestMultiDoc2DialHistorySnapshotsIncludeGoldAgentAnswers(t *testing.T) {
	turns := []multiDoc2DialTurn{
		{Role: "user", TurnID: 1, Utterance: "I need to remove a lienholder."},
		{Role: "agent", TurnID: 2, Utterance: "Is there a lienholder on the title?"},
		{Role: "user", TurnID: 3, Utterance: "Yes, there is."},
		{Role: "agent", TurnID: 4, Utterance: "Have you recently sold the vehicle?"},
		{Role: "user", TurnID: 5, Utterance: "Not right now."},
	}

	snapshots := multiDoc2DialHistorySnapshots(turns)
	if len(snapshots[1]) != 0 {
		t.Fatalf("first user history = %+v", snapshots[1])
	}
	wantSecond := []dialogue.Turn{
		dialogue.NewTurn("user", turns[0].Utterance),
		dialogue.NewTurn("assistant", turns[1].Utterance),
	}
	assertDialogueHistory(t, snapshots[3], wantSecond)
	wantThird := append(append([]dialogue.Turn{}, wantSecond...),
		dialogue.NewTurn("user", turns[2].Utterance),
		dialogue.NewTurn("assistant", turns[3].Utterance),
	)
	assertDialogueHistory(t, snapshots[5], wantThird)
}

func assertDialogueHistory(t *testing.T, got, want []dialogue.Turn) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("history = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("history[%d] = %+v, want %+v", index, got[index], want[index])
		}
	}
}
