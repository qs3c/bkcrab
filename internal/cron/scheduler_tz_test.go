package cron

import (
	"testing"
	"time"
)

// "0 9 * * *" stored with Asia/Shanghai must fire at 09:00 Beijing time
// (01:00 UTC), not at 09:00 server/UTC time — the original bug where a
// 东八区 chatter's "每天早上 9 点" reminder arrived at 下午 5 点.
func TestNextOccurrenceInHonorsLocation(t *testing.T) {
	sh, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load Asia/Shanghai: %v", err)
	}
	// 2026-06-05 00:00 UTC = 08:00 Beijing — next "0 9 * * *" in
	// Shanghai is 09:00 Beijing = 01:00 UTC the same day.
	after := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)

	got := NextOccurrenceIn("0 9 * * *", after, sh)
	want := time.Date(2026, 6, 5, 9, 0, 0, 0, sh)
	if !got.Equal(want) {
		t.Errorf("Shanghai: got %v, want %v", got, want)
	}

	// Same instant evaluated in UTC lands 8 hours later on the wall.
	gotUTC := NextOccurrenceIn("0 9 * * *", after, time.UTC)
	wantUTC := time.Date(2026, 6, 5, 9, 0, 0, 0, time.UTC)
	if !gotUTC.Equal(wantUTC) {
		t.Errorf("UTC: got %v, want %v", gotUTC, wantUTC)
	}
	if gotUTC.Sub(got) != 8*time.Hour {
		t.Errorf("expected 8h gap between UTC and Shanghai fire, got %v", gotUTC.Sub(got))
	}
}

func TestNextOccurrenceInNilLocation(t *testing.T) {
	after := time.Date(2026, 6, 5, 0, 30, 0, 0, time.UTC)
	got := NextOccurrenceIn("*/5 * * * *", after, nil)
	if !got.After(after) {
		t.Errorf("expected occurrence after %v, got %v", after, got)
	}
}

func TestLocationOf(t *testing.T) {
	if loc := LocationOf(""); loc != time.Local {
		t.Errorf("empty name: want time.Local, got %v", loc)
	}
	// Legacy rows were hardcoded "UTC" — they must keep UTC semantics.
	if loc := LocationOf("UTC"); loc != time.UTC {
		t.Errorf("UTC: want time.UTC, got %v", loc)
	}
	if loc := LocationOf("Asia/Shanghai"); loc.String() != "Asia/Shanghai" {
		t.Errorf("Asia/Shanghai: got %v", loc)
	}
	if loc := LocationOf("Not/AZone"); loc != time.Local {
		t.Errorf("unknown name: want time.Local fallback, got %v", loc)
	}
}
