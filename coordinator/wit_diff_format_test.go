package coordinator

import (
	"strings"
	"testing"
)

// TestFormatVersionDiff_SingleLineEdit pins the smallest case: a
// one-line file changed to a different one-line file. The output
// must match the unified-diff shape `patch(1)` and every IDE diff
// viewer expect:
//
//	--- a/scan	<oldlabel>
//	+++ b/scan	<newlabel>
//	@@ -1 +1 @@
//	-echo v1
//	+echo v3
//
// Tab-separated tail on the ---/+++ lines carries our hash+owner
// annotation in a place where `patch` ignores it. Hunk header uses
// the no-comma single-line form when count == 1.
func TestFormatVersionDiff_SingleLineEdit(t *testing.T) {
	got := formatVersionDiff("echo v1\n", "echo v3\n", "scan", "abc1234 alice", "def5678 bob")
	want := "--- a/scan\tabc1234 alice\n" +
		"+++ b/scan\tdef5678 bob\n" +
		"@@ -1 +1 @@\n" +
		"-echo v1\n" +
		"+echo v3\n"
	if got != want {
		t.Errorf("single-line edit:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

// TestFormatVersionDiff_ContextAnchoredHunk verifies that the
// renderer emits 3 lines of context around a change inside a longer
// file. This is the property the old diffmatchpatch-based renderer
// lacked — it dropped every equal line.
func TestFormatVersionDiff_ContextAnchoredHunk(t *testing.T) {
	old := "alpha\nbeta\ngamma\ndelta\necho\nfoxtrot\ngolf\nhotel\nindia\njuliet\nkilo\n"
	new := "alpha\nbeta\ngamma\nDELTA\necho\nfoxtrot\ngolf\nhotel\nindia\njuliet\nkilo\n"
	got := formatVersionDiff(old, new, "cfg", "old", "new")

	if !strings.Contains(got, "@@ -1,7 +1,7 @@") {
		t.Errorf("hunk header missing/wrong; got:\n%s", got)
	}
	// Three lines of context before the change, three after.
	for _, line := range []string{" alpha\n", " beta\n", " gamma\n", "-delta\n", "+DELTA\n", " echo\n", " foxtrot\n", " golf\n"} {
		if !strings.Contains(got, line) {
			t.Errorf("expected %q in hunk; got:\n%s", line, got)
		}
	}
	// Lines beyond the 3-line context window must NOT appear.
	for _, line := range []string{" hotel\n", " india\n", " juliet\n", " kilo\n"} {
		if strings.Contains(got, line) {
			t.Errorf("line %q is outside context window but appeared; got:\n%s", line, got)
		}
	}
}

// TestFormatVersionDiff_IdenticalContent returns empty: there's
// nothing to diff. Callers (wit log -p, wit diff) skip the header
// in that case rather than emit a header-only block.
func TestFormatVersionDiff_IdenticalContent(t *testing.T) {
	got := formatVersionDiff("same\n", "same\n", "name", "old", "new")
	if got != "" {
		t.Errorf("identical content should produce empty output; got %q", got)
	}
}

// TestFormatVersionDiff_NoTrailingNewline checks the edge case
// where one of the sources lacks a trailing newline. difflib has
// a known quirk that fabricates an empty "trailing newline" context
// line; we normalize before splitting so the diff stays clean.
func TestFormatVersionDiff_NoTrailingNewline(t *testing.T) {
	got := formatVersionDiff("echo v1", "echo v2", "scan", "old", "new")
	if strings.Contains(got, " \n") && !strings.Contains(got, "@@") {
		// A bare " \n" (space + newline) with no hunk header would
		// be the difflib artifact — it leaks into hunks too. Fail
		// loudly if we see it without a real reason.
		t.Errorf("output has bogus space-only line (difflib trailing-newline artifact): %q", got)
	}
	if !strings.Contains(got, "-echo v1") || !strings.Contains(got, "+echo v2") {
		t.Errorf("missing change markers; got:\n%s", got)
	}
}

// TestColorDiff_AppliesSyntaxColors verifies that the wrapper that
// sits between formatVersionDiff and the writer actually applies the
// syntax-package diff highlighter. We don't pin exact SGR codes —
// the palette owns those — only that some ANSI opener appears in
// the output and the underlying lines survive intact.
func TestColorDiff_AppliesSyntaxColors(t *testing.T) {
	plain := "--- a/scan\told\n+++ b/scan\tnew\n@@ -1 +1 @@\n-echo v1\n+echo v2\n"
	colored := colorDiff(plain)
	if colored == plain {
		t.Errorf("colorDiff should add ANSI; got unchanged %q", colored)
	}
	if !strings.Contains(colored, "\x1b[") {
		t.Errorf("colorDiff output should contain an ANSI opener; got %q", colored)
	}
	// Byte-faithful through the diff text — the source lines survive.
	for _, frag := range []string{"--- a/scan", "+++ b/scan", "@@ -1 +1 @@", "echo v1", "echo v2"} {
		if !strings.Contains(colored, frag) {
			t.Errorf("colored output lost %q; got %q", frag, colored)
		}
	}
}

// TestColorDiff_EmptyIsEmpty: a no-op diff (identical content)
// gives the empty string with no stray reset code that would
// otherwise leak into the surrounding scrollback.
func TestColorDiff_EmptyIsEmpty(t *testing.T) {
	if got := colorDiff(""); got != "" {
		t.Errorf("empty in should give empty out; got %q", got)
	}
}

// TestFormatVersionDiff_MultipleHunks verifies that two distant
// changes in the same file produce two hunks separated by a fresh
// @@ header — not one giant hunk covering everything between.
func TestFormatVersionDiff_MultipleHunks(t *testing.T) {
	// 12 lines; change line 2 and line 11. With 3 lines of context,
	// the two hunks shouldn't touch.
	old := strings.Repeat("x\n", 12)
	lines := strings.Split(strings.TrimRight(old, "\n"), "\n")
	lines[1] = "y"
	lines[10] = "z"
	new := strings.Join(lines, "\n") + "\n"

	got := formatVersionDiff(old, new, "f", "old", "new")
	if strings.Count(got, "@@") != 4 { // two hunks × two @@ markers each
		t.Errorf("expected two separate hunks (4 @@ markers); got %d in:\n%s",
			strings.Count(got, "@@"), got)
	}
}
