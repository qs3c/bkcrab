package imagegen

import "testing"

func TestPlanDeterministicChunks(t *testing.T) {
	for _, tt := range []struct {
		count int
		want  []int
	}{{1, []int{1}}, {4, []int{4}}, {5, []int{4, 1}}, {9, []int{4, 4, 1}}, {16, []int{4, 4, 4, 4}}} {
		req := NormalizedRequest{Version: RequestSchemaVersion, Action: ActionCreate, Items: []NormalizedItem{{Index: 0, Label: "item-0", Prompt: "A", Size: SizeSquare, Count: tt.count}}}
		got, err := PlanTasks(req, 4)
		if err != nil {
			t.Fatalf("PlanTasks(%d) error = %v", tt.count, err)
		}
		if len(got) != len(tt.want) {
			t.Fatalf("PlanTasks(%d) length = %d, want %d", tt.count, len(got), len(tt.want))
		}
		for i := range got {
			if got[i].ItemIndex != 0 || got[i].ChunkIndex != i || got[i].RequestedCount != tt.want[i] || len(got[i].RequestFingerprint) != 64 {
				t.Fatalf("PlanTasks(%d)[%d] = %#v", tt.count, i, got[i])
			}
		}
	}
}

func TestPlanDoesNotMergeItemsAndFingerprintIsStable(t *testing.T) {
	req := NormalizedRequest{Version: RequestSchemaVersion, Action: ActionCreate, Items: []NormalizedItem{
		{Index: 0, Label: "cover", Prompt: "A", Size: SizeLandscape, Count: 5},
		{Index: 1, Label: "avatar", Prompt: "B", Size: SizeSquare, Count: 3},
	}}
	first, err := PlanTasks(req, 4)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanTasks(req, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 || first[0].RequestedCount != 4 || first[1].RequestedCount != 1 || first[2].RequestedCount != 3 || first[2].ItemIndex != 1 || first[2].ChunkIndex != 0 {
		t.Fatalf("planned tasks = %#v", first)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("plan is not deterministic: first=%#v second=%#v", first, second)
		}
	}
}
