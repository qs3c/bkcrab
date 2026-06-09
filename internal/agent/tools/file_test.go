package tools

import (
	"strings"
	"testing"
)

// TestApplyEdit pins the contract that edit_file's three backends share:
// a single match replaces in place, the empty / equal / not-found / multi-
// match cases each error with a fragment the LLM can act on, and
// replace_all swaps every occurrence. Pure-function tests only — backend
// routing is exercised through the running agent.
func TestApplyEdit(t *testing.T) {
	const (
		path  = "MEMORY.md"
		oldS  = "alpha"
		newS  = "beta"
		multi = "alpha and alpha"
	)

	cases := []struct {
		name       string
		content    string
		oldStr     string
		newStr     string
		replaceAll bool

		wantContent string
		wantCount   int
		wantErrSub  string // substring; empty == expect no error
	}{
		{
			name:        "single match replaces in place",
			content:     "x alpha y",
			oldStr:      oldS,
			newStr:      newS,
			wantContent: "x beta y",
			wantCount:   1,
		},
		{
			name:        "replace_all swaps every occurrence",
			content:     multi,
			oldStr:      oldS,
			newStr:      newS,
			replaceAll:  true,
			wantContent: "beta and beta",
			wantCount:   2,
		},
		{
			name:       "multi match without replace_all errors with count and hint",
			content:    multi,
			oldStr:     oldS,
			newStr:     newS,
			wantErrSub: "matches 2 locations",
		},
		{
			name:       "not found errors with path so the LLM knows which file to re-read",
			content:    "nothing here",
			oldStr:     oldS,
			newStr:     newS,
			wantErrSub: "not found in " + path,
		},
		{
			name:       "empty old_string rejected (use write_file instead)",
			content:    "anything",
			oldStr:     "",
			newStr:     newS,
			wantErrSub: "old_string is empty",
		},
		{
			name:       "no-op edit (old == new) rejected",
			content:    "x alpha y",
			oldStr:     oldS,
			newStr:     oldS,
			wantErrSub: "must differ",
		},
		{
			name:        "replace_all with single match still works",
			content:     "x alpha y",
			oldStr:      oldS,
			newStr:      newS,
			replaceAll:  true,
			wantContent: "x beta y",
			wantCount:   1,
		},
		{
			name:        "whitespace-sensitive match (indentation matters)",
			content:     "  alpha\n",
			oldStr:      "  alpha",
			newStr:      "  beta",
			wantContent: "  beta\n",
			wantCount:   1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, count, err := applyEdit(path, tc.content, tc.oldStr, tc.newStr, tc.replaceAll)

			if tc.wantErrSub != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (content=%q)", tc.wantErrSub, got)
				}
				if !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErrSub)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantContent {
				t.Errorf("content mismatch:\n  got:  %q\n  want: %q", got, tc.wantContent)
			}
			if count != tc.wantCount {
				t.Errorf("count mismatch: got %d, want %d", count, tc.wantCount)
			}
		})
	}
}
