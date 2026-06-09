package tools

import (
	"context"
	"strings"
	"testing"
)

// Pure-function tests: parser + applier. Backend integration (workspace
// store / system file store / sandbox executor) is exercised through the
// running agent — same scope split as file_test.go's TestApplyEdit.

func TestParsePatch_Basics(t *testing.T) {
	in := `*** Begin Patch
*** Add File: foo.txt
+hello
+world
*** Update File: bar.txt
@@
 ctx
-old
+new
*** Delete File: legacy.txt
*** End Patch`

	p, err := parsePatch(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := len(p.Ops), 3; got != want {
		t.Fatalf("ops count: got %d want %d", got, want)
	}

	add := p.Ops[0]
	if add.Type != opAdd || add.Path != "foo.txt" || add.AddBody != "hello\nworld\n" {
		t.Errorf("add: %+v", add)
	}

	upd := p.Ops[1]
	if upd.Type != opUpdate || upd.Path != "bar.txt" || len(upd.Hunks) != 1 {
		t.Errorf("update: %+v", upd)
	}
	want := []hunkLine{
		{Kind: lineContext, Text: "ctx"},
		{Kind: lineRemove, Text: "old"},
		{Kind: lineAdd, Text: "new"},
	}
	if got := upd.Hunks[0].Lines; !linesEq(got, want) {
		t.Errorf("hunk lines: got %+v want %+v", got, want)
	}

	del := p.Ops[2]
	if del.Type != opDelete || del.Path != "legacy.txt" {
		t.Errorf("delete: %+v", del)
	}
}

func TestParsePatch_MoveAndEOF(t *testing.T) {
	in := `*** Begin Patch
*** Update File: a.go
*** Move to: b.go
@@
 keep
-bye
+hi
*** End of File
*** End Patch`
	p, err := parsePatch(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	op := p.Ops[0]
	if op.MoveTo != "b.go" {
		t.Errorf("move target: %q", op.MoveTo)
	}
	if !op.Hunks[0].IsEOF {
		t.Errorf("hunk should be EOF-anchored")
	}
}

func TestParsePatch_MultipleHunks(t *testing.T) {
	in := `*** Begin Patch
*** Update File: x.txt
@@
 a
-b
+B
@@
 c
-d
+D
*** End Patch`
	p, err := parsePatch(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := len(p.Ops[0].Hunks); got != 2 {
		t.Errorf("hunks: %d", got)
	}
}

func TestParsePatch_ToleratesBlanks(t *testing.T) {
	// Leading whitespace, blank lines between Begin Patch and first op,
	// trailing newline. All common LLM behaviors.
	in := "  \n\n*** Begin Patch\n\n*** Add File: f\n+x\n*** End Patch\n"
	p, err := parsePatch(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if p.Ops[0].AddBody != "x\n" {
		t.Errorf("body: %q", p.Ops[0].AddBody)
	}
}

func TestParsePatch_Errors(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantSub string
	}{
		{
			name:    "missing begin",
			input:   "*** Add File: f\n+x\n*** End Patch",
			wantSub: "Begin Patch",
		},
		{
			name:    "missing end",
			input:   "*** Begin Patch\n*** Add File: f\n+x\n",
			wantSub: "End Patch",
		},
		{
			name:    "move outside update",
			input:   "*** Begin Patch\n*** Move to: y\n*** End Patch",
			wantSub: "outside an Update File",
		},
		{
			name:    "hunk outside update",
			input:   "*** Begin Patch\n@@\n+x\n*** End Patch",
			wantSub: "outside an Update File",
		},
		{
			name:    "EOF outside hunk",
			input:   "*** Begin Patch\n*** Update File: f\n*** End of File\n*** End Patch",
			wantSub: "not inside an Update hunk",
		},
		{
			name:    "Add body without plus",
			input:   "*** Begin Patch\n*** Add File: f\nno plus\n*** End Patch",
			wantSub: "must start with",
		},
		{
			name:    "bad hunk prefix",
			input:   "*** Begin Patch\n*** Update File: f\n@@\n!bad\n*** End Patch",
			wantSub: "must start with",
		},
		{
			name:    "empty patch",
			input:   "*** Begin Patch\n*** End Patch",
			wantSub: "empty patch",
		},
		{
			name:    "move after hunk",
			input:   "*** Begin Patch\n*** Update File: f\n@@\n a\n*** Move to: g\n*** End Patch",
			wantSub: "must come before any hunk",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parsePatch(tc.input)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}

func TestApplyHunks_SingleHunk(t *testing.T) {
	old := "alpha\nbeta\ngamma\n"
	hunks := []hunk{{
		Lines: []hunkLine{
			{Kind: lineContext, Text: "alpha"},
			{Kind: lineRemove, Text: "beta"},
			{Kind: lineAdd, Text: "BETA"},
			{Kind: lineContext, Text: "gamma"},
		},
	}}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "alpha\nBETA\ngamma\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestApplyHunks_MultipleHunksAdvanceAnchor(t *testing.T) {
	// Two hunks anchored on the same context line; the applier must advance
	// past the first match so the second hunk lands on the second occurrence.
	old := "x\nctx\na\nctx\nb\n"
	hunks := []hunk{
		{Lines: []hunkLine{
			{Kind: lineContext, Text: "ctx"},
			{Kind: lineRemove, Text: "a"},
			{Kind: lineAdd, Text: "A"},
		}},
		{Lines: []hunkLine{
			{Kind: lineContext, Text: "ctx"},
			{Kind: lineRemove, Text: "b"},
			{Kind: lineAdd, Text: "B"},
		}},
	}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "x\nctx\nA\nctx\nB\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestApplyHunks_EOFAppend(t *testing.T) {
	old := "a\nb\nc\n"
	hunks := []hunk{{
		IsEOF: true,
		Lines: []hunkLine{
			{Kind: lineContext, Text: "c"},
			{Kind: lineAdd, Text: "d"},
		},
	}}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "a\nb\nc\nd\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestApplyHunks_FuzzyTrailingWS(t *testing.T) {
	// File has trailing spaces the patch context did not.
	old := "alpha   \nbeta\n"
	hunks := []hunk{{
		Lines: []hunkLine{
			{Kind: lineContext, Text: "alpha"},
			{Kind: lineRemove, Text: "beta"},
			{Kind: lineAdd, Text: "BETA"},
		},
	}}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Replacement uses the trimmed-pattern position, so the original
	// trailing-space line is preserved verbatim.
	if want := "alpha   \nBETA\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestApplyHunks_NoMatch(t *testing.T) {
	old := "alpha\nbeta\n"
	hunks := []hunk{{
		Lines: []hunkLine{
			{Kind: lineContext, Text: "missing"},
			{Kind: lineAdd, Text: "x"},
		},
	}}
	_, err := applyHunks("foo.go", old, hunks)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "did not match") || !strings.Contains(err.Error(), "foo.go") {
		t.Errorf("unhelpful error: %v", err)
	}
}

func TestApplyHunks_PreservesNoTrailingNewline(t *testing.T) {
	old := "a\nb" // no trailing \n
	hunks := []hunk{{
		Lines: []hunkLine{
			{Kind: lineContext, Text: "a"},
			{Kind: lineRemove, Text: "b"},
			{Kind: lineAdd, Text: "B"},
		},
	}}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "a\nB"; got != want {
		t.Errorf("got %q want %q (trailing-newline state must be preserved)", got, want)
	}
}

// Borrowed from openai/codex codex-rs/apply-patch:
// test_pure_addition_chunk_followed_by_removal. A pure-add hunk (no
// context, no remove) is implicitly EOF-anchored and must not consume
// the search cursor — otherwise the following anchored hunk can no
// longer find its match at the head of the file.
func TestApplyHunks_PureAddBeforeAnchoredHunk(t *testing.T) {
	old := "line1\nline2\nline3\n"
	hunks := []hunk{
		{Lines: []hunkLine{
			{Kind: lineAdd, Text: "after-context"},
			{Kind: lineAdd, Text: "second-line"},
		}},
		{Lines: []hunkLine{
			{Kind: lineContext, Text: "line1"},
			{Kind: lineRemove, Text: "line2"},
			{Kind: lineRemove, Text: "line3"},
			{Kind: lineAdd, Text: "line2-replacement"},
		}},
	}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "line1\nline2-replacement\nafter-context\nsecond-line\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// Borrowed from openai/codex test_update_file_hunk_interleaved_changes:
// three hunks on disjoint regions, the last one EOF-anchored. Stresses
// search-cursor advancement and EOF interplay.
func TestApplyHunks_Interleaved(t *testing.T) {
	old := "a\nb\nc\nd\ne\nf\n"
	hunks := []hunk{
		{Lines: []hunkLine{
			{Kind: lineContext, Text: "a"},
			{Kind: lineRemove, Text: "b"},
			{Kind: lineAdd, Text: "B"},
		}},
		{Lines: []hunkLine{
			{Kind: lineContext, Text: "c"},
			{Kind: lineContext, Text: "d"},
			{Kind: lineRemove, Text: "e"},
			{Kind: lineAdd, Text: "E"},
		}},
		{IsEOF: true, Lines: []hunkLine{
			{Kind: lineContext, Text: "f"},
			{Kind: lineAdd, Text: "g"},
		}},
	}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "a\nB\nc\nd\nE\nf\ng\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// Borrowed from openai/codex test_update_line_with_unicode_dash:
// the file's line uses U+2013 (en-dash) and U+2011 (non-breaking
// hyphen) where the patch wrote ASCII '-'. Codex's third-level fuzzy
// matcher normalises typographic glyphs before comparing, so the patch
// still anchors. We follow the same rule.
func TestApplyHunks_UnicodeDash(t *testing.T) {
	old := "import asyncio  # local import – avoids top‑level dep\n"
	hunks := []hunk{{Lines: []hunkLine{
		{Kind: lineRemove, Text: "import asyncio  # local import - avoids top-level dep"},
		{Kind: lineAdd, Text: "import asyncio  # HELLO"},
	}}}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "import asyncio  # HELLO\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// Full trim() fuzzy level: file has leading indentation that the patch
// context dropped (or vice versa). rstrip alone wouldn't catch this.
func TestApplyHunks_LeadingWhitespaceFuzzy(t *testing.T) {
	old := "    func foo() {\n    return 1\n    }\n"
	// Patch context is un-indented (model normalized whitespace away).
	hunks := []hunk{{Lines: []hunkLine{
		{Kind: lineContext, Text: "func foo() {"},
		{Kind: lineRemove, Text: "return 1"},
		{Kind: lineAdd, Text: "return 2"},
		{Kind: lineContext, Text: "}"},
	}}}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Context lines come from the file (preserved indentation); only
	// the add line uses the patch's text. Result: file is reindented
	// where the patch contributed a new line, but context is intact.
	if want := "    func foo() {\nreturn 2\n    }\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// EOF hunk's end-of-file position fails to match — Codex falls back to
// a full-file forward scan. We follow.
func TestApplyHunks_EOFFallbackToForwardScan(t *testing.T) {
	// File: anchor is at the start, but hunk is mistakenly tagged EOF.
	// Codex (and us) recover by scanning from searchFrom.
	old := "anchor\nbody\ntail\n"
	hunks := []hunk{{
		IsEOF: true,
		Lines: []hunkLine{
			{Kind: lineContext, Text: "anchor"},
			{Kind: lineAdd, Text: "inserted"},
		},
	}}
	got, err := applyHunks("test", old, hunks)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := "anchor\ninserted\nbody\ntail\n"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

// Borrowed from openai/codex test_update_file_hunk_can_move_file:
// Update + Move to: in one op. Source must be deleted, destination has
// the patched content. Verified at the runApplyPatch level (Move is
// implemented as write-target + delete-source).
func TestRunApplyPatch_Move(t *testing.T) {
	files := map[string]string{"src.txt": "line\n"}
	read := func(_ context.Context, p string) (string, error) { return files[p], nil }
	write := func(_ context.Context, p, c string) error { files[p] = c; return nil }
	del := func(_ context.Context, p string) error { delete(files, p); return nil }

	patch := `*** Begin Patch
*** Update File: src.txt
*** Move to: dst.txt
@@
-line
+line2
*** End Patch`
	if _, err := runApplyPatch(context.Background(), patch, read, write, del); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, ok := files["src.txt"]; ok {
		t.Errorf("src.txt should be removed after move")
	}
	if files["dst.txt"] != "line2\n" {
		t.Errorf("dst.txt: %q", files["dst.txt"])
	}
}

func TestRunApplyPatch_HappyPath(t *testing.T) {
	// In-memory backend simulating fs.
	files := map[string]string{
		"old.txt":    "alpha\nbeta\ngamma\n",
		"existing.x": "keep\n",
	}
	read := func(_ context.Context, p string) (string, error) {
		s, ok := files[p]
		if !ok {
			return "", nil
		}
		return s, nil
	}
	write := func(_ context.Context, p, c string) error {
		files[p] = c
		return nil
	}
	del := func(_ context.Context, p string) error {
		delete(files, p)
		return nil
	}

	patch := `*** Begin Patch
*** Add File: new.txt
+hello
*** Update File: old.txt
@@
 alpha
-beta
+BETA
 gamma
*** Delete File: existing.x
*** End Patch`

	out, err := runApplyPatch(context.Background(), patch, read, write, del)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(out, "A new.txt") || !strings.Contains(out, "U old.txt") || !strings.Contains(out, "D existing.x") {
		t.Errorf("summary missing entries: %q", out)
	}
	if files["new.txt"] != "hello\n" {
		t.Errorf("new.txt: %q", files["new.txt"])
	}
	if files["old.txt"] != "alpha\nBETA\ngamma\n" {
		t.Errorf("old.txt: %q", files["old.txt"])
	}
	if _, ok := files["existing.x"]; ok {
		t.Errorf("existing.x not deleted")
	}
}

func TestRunApplyPatch_AtomicOnHunkFail(t *testing.T) {
	// The first op succeeds in memory but the second op's hunk does not
	// match. NO write should be flushed: the in-memory plan is built
	// completely before any write fires.
	files := map[string]string{
		"a.txt": "keep\n",
		"b.txt": "alpha\nbeta\n",
	}
	read := func(_ context.Context, p string) (string, error) {
		s := files[p]
		return s, nil
	}
	writes := 0
	write := func(_ context.Context, p, c string) error {
		writes++
		files[p] = c
		return nil
	}
	del := func(_ context.Context, p string) error {
		delete(files, p)
		return nil
	}

	patch := `*** Begin Patch
*** Add File: created.txt
+x
*** Update File: b.txt
@@
 NOT_THERE
-beta
+BETA
*** End Patch`

	if _, err := runApplyPatch(context.Background(), patch, read, write, del); err == nil {
		t.Fatal("expected error")
	}
	if writes != 0 {
		t.Errorf("expected 0 writes on hunk failure, got %d", writes)
	}
	if _, ok := files["created.txt"]; ok {
		t.Errorf("created.txt should not exist (atomicity broken)")
	}
}

// =====================================================================
// Direct unit tests for matching primitives.
//
// These cover seekSequence and normalizeForFuzzy at the granularity
// Codex tests them in seek_sequence.rs / parser.rs. applyHunks-level
// tests already exercise the integrated path; these guard the helper
// contracts so a regression in normalisation or anchor scanning shows
// up here directly.
// =====================================================================

func TestSeekSequence(t *testing.T) {
	cases := []struct {
		name     string
		haystack []string
		pattern  []string
		start    int
		want     int
	}{
		{
			name:     "exact match",
			haystack: []string{"a", "b", "c", "d"},
			pattern:  []string{"b", "c"},
			want:     1,
		},
		{
			name:     "first match wins among duplicates",
			haystack: []string{"x", "ctx", "y", "ctx", "z"},
			pattern:  []string{"ctx"},
			want:     1,
		},
		{
			name:     "start offset skips earlier occurrences",
			haystack: []string{"x", "ctx", "y", "ctx", "z"},
			pattern:  []string{"ctx"},
			start:    2,
			want:     3,
		},
		{
			name:     "not found returns -1",
			haystack: []string{"a", "b"},
			pattern:  []string{"missing"},
			want:     -1,
		},
		{
			name:     "pattern longer than haystack returns -1",
			haystack: []string{"a"},
			pattern:  []string{"a", "b"},
			want:     -1,
		},
		{
			name:     "empty pattern returns start",
			haystack: []string{"a", "b"},
			pattern:  nil,
			start:    1,
			want:     1,
		},
		{
			name:     "empty pattern at end returns end",
			haystack: []string{"a"},
			pattern:  nil,
			start:    1,
			want:     1,
		},
		{
			name:     "start past end returns -1",
			haystack: []string{"a"},
			pattern:  []string{"a"},
			start:    5,
			want:     -1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := seekSequence(tc.haystack, tc.pattern, tc.start); got != tc.want {
				t.Errorf("got %d want %d", got, tc.want)
			}
		})
	}
}

func TestNormalizeForFuzzy(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		// \u-escaped where the glyph matters so an auto-formatter can't
		// silently rewrite the input back to ASCII and hide what's being
		// tested.
		{"ASCII fast path unchanged", "hello world", "hello world"},
		{"ASCII trim", "  hello  ", "hello"},
		{"en-dash U+2013", "foo–bar", "foo-bar"},
		{"em-dash U+2014", "foo—bar", "foo-bar"},
		{"non-breaking hyphen U+2011", "top‑level", "top-level"},
		{"figure dash U+2012", "a‒b", "a-b"},
		{"horizontal bar U+2015", "a―b", "a-b"},
		{"minus sign U+2212", "a−b", "a-b"},
		{"hyphen U+2010", "a‐b", "a-b"},
		{"left single quote U+2018", "it‘s", "it's"},
		{"right single quote U+2019", "it’s", "it's"},
		{"low-9 single quote U+201A", "‚oops", "'oops"},
		{"high-reversed-9 quote U+201B", "‛oops", "'oops"},
		{"left double quote U+201C", "say “hi”", `say "hi"`},
		{"low-9 double quote U+201E", "„hi“", `"hi"`},
		{"high-reversed double quote U+201F", "‟hi", `"hi`},
		{"non-breaking space U+00A0", "a b", "a b"},
		{"figure space U+2007", "a b", "a b"},
		{"thin space U+2009", "a b", "a b"},
		{"narrow no-break space U+202F", "a b", "a b"},
		{"ideographic space U+3000", "a　b", "a b"},
		{"mixed glyphs", "—it’s “ok” now", `-it's "ok" now`},
		{"non-mapped non-ASCII passes through", "日本語", "日本語"},
		{"non-mapped non-ASCII trimmed", "  日本語  ", "日本語"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeForFuzzy(tc.in); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// Parser case: the first hunk in an Update File block can omit the @@
// header — Codex's parser.rs allows this in lenient mode.
func TestParsePatch_FirstHunkNoAtAt(t *testing.T) {
	in := `*** Begin Patch
*** Update File: f.go
 ctx
-old
+new
*** End Patch`
	p, err := parsePatch(in)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := len(p.Ops[0].Hunks); got != 1 {
		t.Fatalf("hunks: %d", got)
	}
	want := []hunkLine{
		{Kind: lineContext, Text: "ctx"},
		{Kind: lineRemove, Text: "old"},
		{Kind: lineAdd, Text: "new"},
	}
	if got := p.Ops[0].Hunks[0].Lines; !linesEq(got, want) {
		t.Errorf("got %+v want %+v", got, want)
	}
}

// Engine-level identity-file refusals — both Delete and Move should
// reject the operation BEFORE any backend call fires. Tested via
// runApplyPatch with a noop backend that would otherwise let the op
// through.
func TestRunApplyPatch_IdentityRefusals(t *testing.T) {
	noopRead := func(_ context.Context, _ string) (string, error) { return "", nil }
	noopWrite := func(_ context.Context, _, _ string) error { return nil }
	noopDel := func(_ context.Context, _ string) error { return nil }

	cases := []struct {
		name    string
		patch   string
		wantSub string
	}{
		{
			name: "delete SOUL.md refused",
			patch: `*** Begin Patch
*** Delete File: SOUL.md
*** End Patch`,
			wantSub: "refusing to delete identity file",
		},
		{
			name: "move FROM identity refused",
			patch: `*** Begin Patch
*** Update File: SOUL.md
*** Move to: x.md
@@
-a
+b
*** End Patch`,
			wantSub: "refusing to Move identity file",
		},
		{
			name: "move TO identity refused",
			patch: `*** Begin Patch
*** Update File: x.md
*** Move to: SOUL.md
@@
-a
+b
*** End Patch`,
			wantSub: "refusing to Move identity file",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runApplyPatch(context.Background(), tc.patch, noopRead, noopWrite, noopDel)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

// linesEq compares two hunkLine slices.
func linesEq(a, b []hunkLine) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
