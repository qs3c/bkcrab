package eval

import "testing"

func TestModelClosedEnums(t *testing.T) {
	if !RunModeFull.Valid() || !RunModeOnlineOnly.Valid() || RunMode("full").Valid() {
		t.Fatal("run mode enum is not closed")
	}
	for _, status := range []MetricStatus{MetricOK, MetricSkippedMissingInput, MetricError} {
		if status == "" {
			t.Fatal("metric status must be explicit")
		}
	}
}
