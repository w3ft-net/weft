package logtable

import (
	"regexp"
	"strings"
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
	got := Name([]any{int64(1)}, "weft", "shard", "stats")
	if want := "l_1_weft_shard_stats_08120e66cd21ef21ddde3ac8f50dff73"; got != want {
		t.Fatalf("Name([]any{int64(1)}, weft, shard, stats) = %q, want %q", got, want)
	}
}

// The readable part is for people looking at a shard. Uniqueness comes from the
// hash after it, because DuckDB compares identifiers without regard to case, so
// two services that differ only in case read the same.
func TestNameIsReadableAndStillSeparatesCase(t *testing.T) {
	lower, upper := Name([]any{int64(1)}, "acme", "api", "main"), Name([]any{int64(1)}, "acme", "Api", "main")
	if !strings.HasPrefix(lower, "l_1_acme_api_main_") || !strings.HasPrefix(upper, "l_1_acme_api_main_") {
		t.Errorf("names %q and %q do not read as the key", lower, upper)
	}
	if strings.ToLower(lower) == strings.ToLower(upper) {
		t.Errorf("%q and %q are the same identifier to DuckDB", lower, upper)
	}
}

func TestNameStaysShortForLongKeys(t *testing.T) {
	long := strings.Repeat("segment/", 40)
	got := Name([]any{int64(12345)}, long, long, long)
	if len(got) > 2+maxSlug+1+32 {
		t.Errorf("name is %d bytes: %q", len(got), got)
	}
	if !IsName(got) {
		t.Errorf("%q is not recognised as a log table", got)
	}
}

func TestIsName(t *testing.T) {
	for _, n := range []string{
		Name([]any{int64(1)}, "weft", "shard", "stats"),
		Name([]any{int64(-5)}, "loom", "loom/api", "main"),
		Name([]any{int64(0)}, "", "", ""),
		Name([]any{int64(7)}, "日本語", `a"b`, "main"),
	} {
		if !IsName(n) {
			t.Errorf("IsName(%q) = false", n)
		}
	}
	for _, n := range []string{
		"logs", "field_facts_state", "l_", "l_foo", "l_1_weft_08120e66cd21ef21ddde3ac8f50dff7",
		"L_1_weft_08120e66cd21ef21ddde3ac8f50dff73", "x_1_weft_08120e66cd21ef21ddde3ac8f50dff73",
	} {
		if IsName(n) {
			t.Errorf("IsName(%q) = true", n)
		}
	}
}

func TestNameIsAnIdentifierForAnyInput(t *testing.T) {
	ident := regexp.MustCompile(`^l_[a-z0-9_]+_[0-9a-f]{32}$`)
	for _, k := range []key{
		{1, "weft", "shard", "main"},
		{0, "", "", ""},
		{-5, "loom", "loom/api", "main"},
		{7, `a"b`, "x; DROP TABLE logs; --", "k'ind"},
		{7, "ünï", "日本語", "main"},
	} {
		if got := Name([]any{k.customer}, k.namespace, k.service, k.kind); !ident.MatchString(got) {
			t.Errorf("Name(%v) = %q, not a bare identifier", k, got)
		}
	}
}

// A layout with other partition columns names tables too, and a boundary can't
// move between them or across to the namespace.
func TestNameSeparatesPartitionValues(t *testing.T) {
	seen := map[string][]any{}
	for _, p := range [][]any{
		nil,
		{int64(1)},
		{"1"},
		{"a", "bc"},
		{"ab", "c"},
		{"a", "b", "c"},
		{int64(1), "a"},
	} {
		got := Name(p, "ns", "svc", "main")
		if prev, dup := seen[got]; dup {
			t.Errorf("%v and %v share the table %s", prev, p, got)
		}
		seen[got] = p
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
		got := Name([]any{k.customer}, k.namespace, k.service, k.kind)
		if prev, dup := seen[got]; dup {
			t.Errorf("%v collides with %v on %s", k, prev, got)
		}
		seen[got] = k
	}
}
