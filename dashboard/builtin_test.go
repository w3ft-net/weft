package dashboard

import (
	"testing"

	"github.com/graybeardcorp/weft/chart"
)

func TestToTaggedWatchSeries_SplitsMultiRowRuns(t *testing.T) {
	// Each run's stdout has one JSON row per coordinator — the shape a
	// tagged watch actually emits (e.g. `runtime_stats | jq -c ...`).
	results := []chart.WatchResultValue{
		{
			RanAt: "2026-01-01T00:02:00Z",
			Value: `{"coordinator_id":"i-aaa","mem_pct":48}
{"coordinator_id":"i-bbb","mem_pct":68}`,
		},
		{
			RanAt: "2026-01-01T00:01:00Z",
			Value: `{"coordinator_id":"i-aaa","mem_pct":44}
{"coordinator_id":"i-bbb","mem_pct":64}`,
		},
		{
			RanAt: "2026-01-01T00:00:00Z",
			Value: `{"coordinator_id":"i-aaa","mem_pct":40}
{"coordinator_id":"i-bbb","mem_pct":60}`,
		},
	}
	ws := toTaggedWatchSeries(results, ".coordinator_id", ".mem_pct")
	if len(ws.Tags) != 2 {
		t.Fatalf("expected 2 tags, got %d: %+v", len(ws.Tags), ws.Tags)
	}
	aaa, ok := ws.Tags["i-aaa"]
	if !ok {
		t.Fatal("missing i-aaa series")
	}
	// Newest-first, matching toWatchSeries' convention.
	if len(aaa.Values) != 3 || aaa.Values[0] != 48 || aaa.Values[2] != 40 {
		t.Errorf("i-aaa values: got %v, want newest-first [48 44 40]", aaa.Values)
	}
	bbb := ws.Tags["i-bbb"]
	if len(bbb.Values) != 3 || bbb.Values[0] != 68 {
		t.Errorf("i-bbb values: got %v, want newest-first starting 68", bbb.Values)
	}
}

func TestToTaggedWatchSeries_SkipsRowsMissingFields(t *testing.T) {
	results := []chart.WatchResultValue{
		{
			RanAt: "2026-01-01T00:00:00Z",
			Value: `{"mem_pct":50}
{"coordinator_id":"i-aaa"}
{"coordinator_id":"i-aaa","mem_pct":40}
not json at all`,
		},
	}
	ws := toTaggedWatchSeries(results, ".coordinator_id", ".mem_pct")
	if len(ws.Tags) != 1 {
		t.Fatalf("expected exactly 1 tag, got %d: %+v", len(ws.Tags), ws.Tags)
	}
	if got := ws.Tags["i-aaa"].Values; len(got) != 1 || got[0] != 40 {
		t.Errorf("i-aaa values: got %v, want [40]", got)
	}
}

func TestToTaggedWatchSeries_EmptyOnNoMatches(t *testing.T) {
	results := []chart.WatchResultValue{
		{RanAt: "2026-01-01T00:00:00Z", Value: `{"other_field":"x"}`},
	}
	ws := toTaggedWatchSeries(results, ".coordinator_id", ".mem_pct")
	if len(ws.Tags) != 0 {
		t.Errorf("expected no tags, got %+v", ws.Tags)
	}
}

func TestParseBuiltinArgs(t *testing.T) {
	for _, c := range []struct {
		args     []string
		name, er string
	}{
		{nil, "", ""},
		{[]string{"prod"}, "prod", ""},
		{[]string{"prod", "--fullscreen"}, "", "unknown option --fullscreen"},
		{[]string{"-x"}, "", "unknown option -x"},
		{[]string{"prod", "staging"}, "", "unexpected argument staging"},
	} {
		name, er := parseBuiltinArgs(c.args)
		if name != c.name || er != c.er {
			t.Errorf("parseBuiltinArgs(%v) = %q, %q; want %q, %q", c.args, name, er, c.name, c.er)
		}
	}
}
