package logtable

import (
	"regexp"
	"testing"
)

type key struct {
	customer           int64
	namespace, service string
	kind               string
}

// The planner and every shard must derive the same name for a key, across
// deploys, so the function is pinned: if this test needs its expected value
// changed, tables already on disk and in flight queries stop matching.
func TestNameIsPinned(t *testing.T) {
	got := Name(1, "weft", "shard", "stats")
	if want := "l_08120e66cd21ef21ddde3ac8f50dff73"; got != want {
		t.Fatalf("Name(1, weft, shard, stats) = %q, want %q", got, want)
	}
}

func TestNameIsAnIdentifierForAnyInput(t *testing.T) {
	ident := regexp.MustCompile(`^l_[0-9a-f]{32}$`)
	for _, k := range []key{
		{1, "weft", "shard", "main"},
		{0, "", "", ""},
		{-5, "loom", "loom/api", "main"},
		{7, `a"b`, "x; DROP TABLE logs; --", "k'ind"},
		{7, "ünï", "日本語", "main"},
	} {
		if got := Name(k.customer, k.namespace, k.service, k.kind); !ident.MatchString(got) {
			t.Errorf("Name(%v) = %q, not a bare identifier", k, got)
		}
	}
}

func TestNameSeparatesEveryField(t *testing.T) {
	seen := map[string]key{}
	for _, k := range []key{
		{1, "weft", "shard", "main"},
		{2, "weft", "shard", "main"},
		{1, "weft2", "shard", "main"},
		{1, "weft", "shard2", "main"},
		{1, "weft", "shard", "stats"},
		// the fields are length-prefixed, so a boundary can't move between them
		{1, "ab", "c", "main"},
		{1, "a", "bc", "main"},
		{1, "a", "b", "cmain"},
		{1, "", "abc", "main"},
	} {
		got := Name(k.customer, k.namespace, k.service, k.kind)
		if prev, dup := seen[got]; dup {
			t.Errorf("%v collides with %v on %s", k, prev, got)
		}
		seen[got] = k
	}
}
