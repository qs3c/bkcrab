package skills

import (
	"math"
	"testing"

	"github.com/qs3c/bkcrab/internal/store"
)

func TestNowSeq(t *testing.T) {
	rows := []store.SkillUsageRow{{Slug: "a", LastLoadSeq: 3}, {Slug: "b", LastLoadSeq: 9}}
	if got := NowSeq(rows); got != 9 {
		t.Fatalf("NowSeq=%d want 9", got)
	}
}

func TestRankKeepsProtectedSkillsAndTopMatureSkills(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "new", Origin: "learner", CreatedSeq: 95},
		{Slug: "edited", Origin: "learner", Activity: 0.1, CreatedSeq: 0, EditedSeq: 80, LastLoadSeq: 80, TotalLoads: 1},
		{Slug: "hot", Origin: "learner", Activity: 10, CreatedSeq: 0, LastLoadSeq: 100, TotalLoads: 4},
		{Slug: "cold", Origin: "learner", Activity: 1, CreatedSeq: 0, LastLoadSeq: 100, TotalLoads: 4},
	}
	active, deletable := Rank(rows, 100, LifecycleConfig{ActiveMax: 1, ProtectLoads: 20, EditProtectLoads: 30})
	for _, slug := range []string{"new", "edited", "hot"} {
		if !active[slug] {
			t.Fatalf("%s should be active; active=%v", slug, active)
		}
	}
	if active["cold"] {
		t.Fatalf("cold should be cooling; active=%v", active)
	}
	if len(deletable) != 0 {
		t.Fatalf("unexpected deletable: %+v", deletable)
	}
}

func TestRankDeletionRequiresNeverLoadedAndUnedited(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "dead", Origin: "learner", CreatedSeq: 0, TotalLoads: 0, EditedSeq: 0},
		{Slug: "used", Origin: "learner", CreatedSeq: 0, TotalLoads: 1},
		{Slug: "edited", Origin: "learner", CreatedSeq: 0, TotalLoads: 0, EditedSeq: 5},
	}
	active, deletable := Rank(rows, 250, LifecycleConfig{DeleteAfterLoads: 200})
	if len(deletable) != 1 || deletable[0] != "dead" {
		t.Fatalf("deletable=%+v want [dead]", deletable)
	}
	if active["dead"] {
		t.Fatalf("deletable skill should not be active")
	}
}

func TestRankTreatsNaNAsZero(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "bad", Origin: "learner", Activity: math.NaN(), CreatedSeq: 0, LastLoadSeq: 50, TotalLoads: 1},
		{Slug: "good", Origin: "learner", Activity: 1, CreatedSeq: 0, LastLoadSeq: 50, TotalLoads: 1},
	}
	active, _ := Rank(rows, 100, LifecycleConfig{ActiveMax: 1, ProtectLoads: 1})
	if !active["good"] || active["bad"] {
		t.Fatalf("NaN row should sink below finite score: %v", active)
	}
}
