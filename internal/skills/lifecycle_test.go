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

func TestRankPrioritizesProtectedSkillsAndTopMatureSkillsWithinHardCap(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "new", Origin: "learner", CreatedSeq: 95},
		{Slug: "edited", Origin: "learner", Activity: 0.1, CreatedSeq: 0, EditedSeq: 80, LastLoadSeq: 80, TotalLoads: 1},
		{Slug: "hot", Origin: "learner", Activity: 10, CreatedSeq: 0, LastLoadSeq: 100, TotalLoads: 4},
		{Slug: "cold", Origin: "learner", Activity: 1, CreatedSeq: 0, LastLoadSeq: 100, TotalLoads: 4},
	}
	active, deletable := Rank(rows, 100, LifecycleConfig{ActiveMax: 3, ProtectLoads: 20, EditProtectLoads: 30})
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

func TestRankCatalogAppliesTierPriorityUnderOneHardCap(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "edited", Origin: "learner", Activity: 0.1, CreatedSeq: 0, EditedSeq: 95, LastLoadSeq: 95, TotalLoads: 1},
		{Slug: "new", Origin: "learner", CreatedSeq: 95},
		{Slug: "hot", Origin: "learner", Activity: 100, CreatedSeq: 0, LastLoadSeq: 100, TotalLoads: 10},
	}
	catalog := []string{"untracked", "hot", "new", "edited"}
	wants := [][]string{
		{"edited"},
		{"edited", "new"},
		{"edited", "new", "hot"},
		{"edited", "new", "hot", "untracked"},
	}
	for i, want := range wants {
		active, _ := RankCatalog(rows, catalog, 100, LifecycleConfig{
			ActiveMax:        i + 1,
			ProtectLoads:     20,
			EditProtectLoads: 30,
		})
		if len(active) != len(want) {
			t.Fatalf("ActiveMax=%d produced %d entries, want %d: %v", i+1, len(active), len(want), active)
		}
		for _, slug := range want {
			if !active[slug] {
				t.Fatalf("ActiveMax=%d should include %q: %v", i+1, slug, active)
			}
		}
	}
}

func TestRankCatalogProtectedEntriesStillObeyAbsoluteHardCap(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "edited-newer", Origin: "learner", EditedSeq: 99, TotalLoads: 1},
		{Slug: "edited-older", Origin: "learner", EditedSeq: 98, TotalLoads: 1},
		{Slug: "new", Origin: "learner", CreatedSeq: 99},
	}
	active, _ := RankCatalog(rows, []string{"new", "edited-older", "edited-newer"}, 100, LifecycleConfig{
		ActiveMax:        2,
		ProtectLoads:     20,
		EditProtectLoads: 30,
	})
	if len(active) != 2 || !active["edited-newer"] || !active["edited-older"] || active["new"] {
		t.Fatalf("all protected entries must share the hard cap; active=%v", active)
	}
}

func TestRankCatalogCountsUntrackedEntriesAndOrdersThemDeterministically(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "tracked-b", Origin: "learner", Activity: 1, CreatedSeq: 0, LastLoadSeq: 100, TotalLoads: 1},
		{Slug: "tracked-a", Origin: "learner", Activity: 1, CreatedSeq: 0, LastLoadSeq: 100, TotalLoads: 1},
	}
	first, _ := RankCatalog(rows,
		[]string{"untracked-z", "tracked-b", "untracked-a", "tracked-a", "untracked-a"},
		100, LifecycleConfig{ActiveMax: 3, ProtectLoads: 1})
	second, _ := RankCatalog([]store.SkillUsageRow{rows[1], rows[0]},
		[]string{"tracked-a", "untracked-a", "tracked-b", "untracked-z"},
		100, LifecycleConfig{ActiveMax: 3, ProtectLoads: 1})

	for _, active := range []map[string]bool{first, second} {
		if len(active) != 3 || !active["tracked-a"] || !active["tracked-b"] || !active["untracked-a"] || active["untracked-z"] {
			t.Fatalf("unexpected deterministic hard-cap result: %v", active)
		}
	}
}

func TestRankCatalogDeletionUsesAllLedgerRowsNotCatalogSubset(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "dead-z", Origin: "learner", CreatedSeq: 0},
		{Slug: "live", Origin: "learner", Activity: 1, CreatedSeq: 0, TotalLoads: 1},
		{Slug: "dead-a", Origin: "learner", CreatedSeq: 0},
	}
	active, deletable := RankCatalog(rows, []string{"live"}, 250, LifecycleConfig{
		ActiveMax:        1,
		ProtectLoads:     1,
		DeleteAfterLoads: 200,
	})
	if len(active) != 1 || !active["live"] {
		t.Fatalf("active=%v want only live", active)
	}
	if len(deletable) != 2 || deletable[0] != "dead-a" || deletable[1] != "dead-z" {
		t.Fatalf("deletable=%v want [dead-a dead-z]", deletable)
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

func TestRankCatalogCapacityCanDeletePreviouslyLoadedLowActivityAsset(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "cold", Origin: "learner", Activity: 0.1, CreatedSeq: 1, LastLoadSeq: 10, TotalLoads: 1},
		{Slug: "hot", Origin: "learner", Activity: 10, CreatedSeq: 1, LastLoadSeq: 10, TotalLoads: 5},
	}
	active, deletable := RankCatalog(rows, []string{"hot", "cold"}, 100, LifecycleConfig{
		ActiveMax:    10,
		AssetMax:     1,
		ProtectLoads: 1,
	})
	if len(deletable) != 1 || deletable[0] != "cold" {
		t.Fatalf("deletable=%v want [cold]", deletable)
	}
	if active["cold"] || !active["hot"] {
		t.Fatalf("capacity-deleted skill must leave active catalog: %v", active)
	}
}

func TestRankCatalogCapacityPreservesProtectedAssetsAndAllowsTemporaryOverflow(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "created-protected", Origin: "learner", CreatedSeq: 95},
		{Slug: "edited-protected", Origin: "learner", CreatedSeq: 1, EditedSeq: 95},
		{Slug: "mature", Origin: "learner", Activity: 1, CreatedSeq: 1, LastLoadSeq: 2, TotalLoads: 1},
	}
	active, deletable := RankCatalog(rows,
		[]string{"mature", "created-protected", "edited-protected"},
		100,
		LifecycleConfig{ActiveMax: 10, AssetMax: 1, ProtectLoads: 20, EditProtectLoads: 20},
	)
	if len(deletable) != 1 || deletable[0] != "mature" {
		t.Fatalf("only mature asset should be capacity-deletable: %v", deletable)
	}
	if len(active) != 2 || !active["created-protected"] || !active["edited-protected"] {
		t.Fatalf("protected assets should temporarily exceed AssetMax: %v", active)
	}
}

func TestRankCatalogCapacitySelectionIsDeterministic(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "charlie", Origin: "learner", Activity: 1, CreatedSeq: 1, LastLoadSeq: 10, TotalLoads: 1},
		{Slug: "alpha", Origin: "learner", Activity: 1, CreatedSeq: 1, LastLoadSeq: 10, TotalLoads: 1},
		{Slug: "bravo", Origin: "learner", Activity: 1, CreatedSeq: 1, LastLoadSeq: 10, TotalLoads: 1},
	}
	_, first := RankCatalog(rows, []string{"bravo", "charlie", "alpha"}, 100, LifecycleConfig{
		ActiveMax: 10, AssetMax: 1, ProtectLoads: 1,
	})
	_, second := RankCatalog([]store.SkillUsageRow{rows[2], rows[0], rows[1]}, []string{"alpha", "bravo", "charlie"}, 100, LifecycleConfig{
		ActiveMax: 10, AssetMax: 1, ProtectLoads: 1,
	})
	want := []string{"alpha", "bravo"}
	for label, got := range map[string][]string{"first": first, "second": second} {
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("%s deletable=%v want %v", label, got, want)
		}
	}
}

func TestRankCatalogCapacityDoesNotDeleteUntrackedAsset(t *testing.T) {
	rows := []store.SkillUsageRow{
		{Slug: "tracked", Origin: "learner", Activity: 1, CreatedSeq: 1, LastLoadSeq: 10, TotalLoads: 1},
	}
	active, deletable := RankCatalog(rows, []string{"untracked", "tracked"}, 100, LifecycleConfig{
		ActiveMax: 2, AssetMax: 1, ProtectLoads: 1,
	})
	if len(deletable) != 0 {
		t.Fatalf("untracked assets must wait for ledger reconciliation: %v", deletable)
	}
	if len(active) != 2 || !active["tracked"] || !active["untracked"] {
		t.Fatalf("untracked asset should still consume ActiveMax capacity: %v", active)
	}
}

func TestNormalizeLifecycleConfigDefaultsNonPositiveAssetMax(t *testing.T) {
	for _, assetMax := range []int{0, -1} {
		got := normalizeLifecycleConfig(LifecycleConfig{AssetMax: assetMax})
		if got.AssetMax != DefaultLifecycleAssetMax {
			t.Fatalf("AssetMax=%d normalized to %d, want %d", assetMax, got.AssetMax, DefaultLifecycleAssetMax)
		}
	}
}
