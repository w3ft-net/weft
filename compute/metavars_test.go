package compute

import (
	"strings"
	"testing"
)

// MergeMetaVars is the fix for "jq's reserved meta-variables ...
// resolve to null when read as output" (docs/known-issues.md): the jq
// compute stage runs as a literal program string piped raw JSON text
// via warp's bash.RunStreaming, so a bare "._host" read can only ever
// resolve if the row's own JSON body actually carries a "_host" key —
// there's no side channel for weft to hand the evaluator per-row
// values out of band, since one bash/jq process handles the whole
// stream.
func TestMergeMetaVars_InjectsEveryAlias(t *testing.T) {
	got := MergeMetaVars(`{"pct":42}`, map[string]string{"instance": "web-1"})

	// Both "_host" and "_instance" alias the "instance" column
	// (planner.JQMetaVars) — a reader of either must see the value.
	for _, want := range []string{`"_host":"web-1"`, `"_instance":"web-1"`, `"pct":42`} {
		if !strings.Contains(got, want) {
			t.Errorf("merged JSON %q missing %q", got, want)
		}
	}
}

func TestMergeMetaVars_NoMetaVarsReturnsRawUnchanged(t *testing.T) {
	raw := `{"pct":42}`
	if got := MergeMetaVars(raw, nil); got != raw {
		t.Errorf("MergeMetaVars(raw, nil) = %q, want unchanged %q", got, raw)
	}
}

// A row whose raw body isn't a JSON object (plain text, a JSON array,
// a bare scalar) can't sensibly carry injected keys — jq would already
// behave oddly against it regardless of this fix, so leave it
// untouched rather than corrupt or error on it.
func TestMergeMetaVars_NonObjectRawReturnsUnchanged(t *testing.T) {
	meta := map[string]string{"instance": "web-1"}
	for _, raw := range []string{
		`not json at all`,
		`[1,2,3]`,
		`"a bare string"`,
		`42`,
		// null unmarshals into a map without an error, leaving it nil. A
		// jq projection the shard computes prints null for a missing
		// field, so a jq stage after it reads this.
		`null`,
	} {
		if got := MergeMetaVars(raw, meta); got != raw {
			t.Errorf("MergeMetaVars(%q, meta) = %q, want unchanged", raw, got)
		}
	}
}

// A real payload key never collides with the injected "_"-prefixed
// alias, so real data always wins — the merge only ever ADDS the
// reserved keys, matching the underscore-prefix convention's whole
// rationale (docs/manual/metavars.md).
func TestMergeMetaVars_DoesNotClobberRealKeys(t *testing.T) {
	got := MergeMetaVars(`{"_host":"already-here","pct":1}`, map[string]string{"instance": "web-1"})
	if !strings.Contains(got, `"_host":"already-here"`) {
		t.Errorf("merged JSON %q clobbered a pre-existing real key", got)
	}
}

func TestFirstStageIsAwk(t *testing.T) {
	cases := []struct {
		stages []string
		want   bool
	}{
		{nil, false},
		{[]string{"grep ERROR", "sort"}, false},
		{[]string{"awk"}, true},
		{[]string{`awk 'HOST == "web-1"'`, "sort"}, true},
		{[]string{"grep ERROR", `awk 'HOST == "web-1"'`}, false},
	}
	for _, tt := range cases {
		if got := firstStageIsAwk(tt.stages); got != tt.want {
			t.Errorf("firstStageIsAwk(%v) = %v, want %v", tt.stages, got, tt.want)
		}
	}
}

func TestAwkVarsFromMeta(t *testing.T) {
	vars := awkVarsFromMeta(map[string]string{"instance": "web-1"})
	if vars["HOST"] != "web-1" || vars["INSTANCE"] != "web-1" {
		t.Errorf("awkVarsFromMeta = %v, want both HOST and INSTANCE set to web-1", vars)
	}
}

func TestAwkVarsFromMeta_EmptyIsNil(t *testing.T) {
	if got := awkVarsFromMeta(nil); got != nil {
		t.Errorf("awkVarsFromMeta(nil) = %v, want nil", got)
	}
}
