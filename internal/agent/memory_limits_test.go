package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/qs3c/bkclaw/internal/store"
)

// TestTruncateRunesChineseBoundary pins the rune-safe truncation: the old byte slice
// (s[:300]) cut multi-byte runes (Chinese) mid-character and produced invalid UTF-8.
// truncateRunes must cut on a rune boundary and stay valid UTF-8.
func TestTruncateRunesChineseBoundary(t *testing.T) {
	s := strings.Repeat("中", 10) // 10 runes, 30 bytes
	got := truncateRunes(s, 4)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated string is not valid UTF-8: %q", got)
	}
	if got != "中中中中..." {
		t.Fatalf("got %q, want 4 runes + ellipsis", got)
	}
	if truncateRunes("中中", 5) != "中中" {
		t.Fatalf("under-limit string must be unchanged")
	}
	if truncateRunes("abcdef", 3) != "abc..." {
		t.Fatalf("ascii truncation wrong: %q", truncateRunes("abcdef", 3))
	}
}

// TestAutoPersistMemoryRespectsPerMessageChars asserts the per-message cap is applied
// at the configured rune count, not the old hardcoded 300.
func TestAutoPersistMemoryRespectsPerMessageChars(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	long := strings.Repeat("x", 1000)
	prov := &fakeExtractProvider{resp: `{"memory_ops":[],"user_ops":[]}`}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: long}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "m", groups, 50, 0); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}
	if strings.Contains(prov.gotPrompt, strings.Repeat("x", 51)) {
		t.Fatalf("per-message cap not applied; prompt has >50 x's")
	}
	if !strings.Contains(prov.gotPrompt, strings.Repeat("x", 50)+"...") {
		t.Fatalf("expected 50-rune truncation with ellipsis")
	}
}

// TestAutoPersistMemoryDefaultsKeepLongMessages proves the default per-message cap is
// well above the old 300: a 500-char message must survive untruncated when 0 is passed.
func TestAutoPersistMemoryDefaultsKeepLongMessages(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	msg500 := strings.Repeat("y", 500)
	prov := &fakeExtractProvider{resp: `{"memory_ops":[],"user_ops":[]}`}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: []store.SessionMessage{{Role: "user", Content: msg500}}}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "m", groups, 0, 0); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}
	if !strings.Contains(prov.gotPrompt, msg500) {
		t.Fatalf("500-char message truncated under default per-message cap; default too small")
	}
}

// TestAutoPersistMemoryRespectsMaxPromptChars asserts the total prompt budget is the
// configured rune count: over budget, later messages are dropped and the marker added.
func TestAutoPersistMemoryRespectsMaxPromptChars(t *testing.T) {
	fs := newFakeMemStore()
	mgr := newTestManager(fs)
	prov := &fakeExtractProvider{resp: `{"memory_ops":[],"user_ops":[]}`}
	var msgs []store.SessionMessage
	for i := range 20 {
		msgs = append(msgs, store.SessionMessage{Role: "user", Content: "message-" + strconv.Itoa(i)})
	}
	groups := []store.TurnGroup{{SessionKey: "s1", Messages: msgs}}

	if err := AutoPersistMemory(context.Background(), mgr, prov, "m", groups, 0, 60); err != nil {
		t.Fatalf("AutoPersistMemory: %v", err)
	}
	if !strings.Contains(prov.gotPrompt, "超出上限") {
		t.Fatalf("expected over-budget marker when prompt exceeds maxPromptChars:\n%s", prov.gotPrompt)
	}
	if strings.Contains(prov.gotPrompt, "message-19") {
		t.Fatalf("messages past the budget should have been dropped")
	}
}
