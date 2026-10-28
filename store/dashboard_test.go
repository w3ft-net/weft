package store

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/chart"
)

func TestParseDashboardYAML_Sections(t *testing.T) {
	body := `
title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        cron: error-rate-hourly
        size: md
      - kind: table
        cron: top-errors-hourly
  - title: Capacity
    tiles:
      - kind: counter
        cron: pending-jobs
        size: sm
`
	spec, err := ParseDashboardYAML(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if spec.Title != "Production" {
		t.Errorf("title: got %q", spec.Title)
	}
	if len(spec.Sections) != 2 {
		t.Fatalf("sections: got %d, want 2", len(spec.Sections))
	}
	if spec.Sections[0].Title != "Errors" {
		t.Errorf("section 0 title: got %q", spec.Sections[0].Title)
	}
	if len(spec.Sections[0].Tiles) != 2 {
		t.Errorf("section 0 tiles: got %d", len(spec.Sections[0].Tiles))
	}
	if spec.Sections[0].Tiles[0].Kind != "sparkline" {
		t.Errorf("tile kind: got %q", spec.Sections[0].Tiles[0].Kind)
	}
}

func TestParseDashboardYAML_FlatTilesShorthand(t *testing.T) {
	body := `
title: Quick
tiles:
  - kind: counter
    cron: pending-jobs
  - kind: sparkline
    cron: error-rate
`
	spec, err := ParseDashboardYAML(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(spec.Sections) != 1 {
		t.Fatalf("expected 1 untitled section, got %d", len(spec.Sections))
	}
	if spec.Sections[0].Title != "" {
		t.Errorf("section title should be empty, got %q", spec.Sections[0].Title)
	}
	if len(spec.Sections[0].Tiles) != 2 {
		t.Errorf("tiles: got %d", len(spec.Sections[0].Tiles))
	}
}

func TestParseDashboardYAML_Errors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"empty", "", "empty"},
		{"no tiles", "title: foo\n", "no tiles"},
		{"unknown kind", "tiles:\n  - kind: pizza\n    cron: foo\n", "unknown kind"},
		{"missing watch", "tiles:\n  - kind: sparkline\n", "missing `cron:`"},
		{"missing kind", "tiles:\n  - cron: foo\n", "missing `kind:`"},
		{"bad size", "tiles:\n  - kind: sparkline\n    cron: foo\n    size: huge\n", "unknown size"},
		{"columns is not a tile key", "tiles:\n  - kind: table\n    cron: foo\n    columns: [host, count]\n", "a tile has no 'columns' key"},
		{"sections + flat tiles", "tiles:\n  - kind: counter\n    cron: foo\nsections:\n  - tiles:\n      - kind: counter\n        cron: bar\n", "cannot mix"},
		{"empty section", "sections:\n  - title: foo\n    tiles: []\n", "no tiles"},
		{"unknown field at top", "title: x\nfoo: bar\ntiles:\n  - kind: counter\n    cron: a\n", "not a valid key"},
		{"unknown field in section", "sections:\n  - heading: Ops\n    tiles:\n      - kind: counter\n        cron: a\n", "'heading' is not a valid key"},
		{"unknown field in tile", "tiles:\n  - kind: counter\n    cron: a\n    color: red\n", "'color' is not a valid key"},
		{"format on gauge", "tiles:\n  - kind: gauge\n    cron: a\n    format: \"%.2f\"\n", "`format:` is only supported on counter"},
		{"format on table", "tiles:\n  - kind: table\n    cron: a\n    format: \"%d\"\n", "`format:` is only supported on counter"},
		{"threshold is not a key", "tiles:\n  - kind: counter\n    cron: a\n    threshold: {crit: 1}\n", "'threshold' is not a valid key"},
		{"format non-string", "tiles:\n  - kind: counter\n    cron: a\n    format: 5\n", "`format:` must contain exactly one %d or %.Nf"},
		{"format no verb", "tiles:\n  - kind: counter\n    cron: a\n    format: ms\n", "`format:` must contain exactly one %d or %.Nf"},
		{"format two verbs", "tiles:\n  - kind: counter\n    cron: a\n    format: \"%d/%d\"\n", "`format:` must contain exactly one %d or %.Nf"},
		{"format unsupported verb", "tiles:\n  - kind: counter\n    cron: a\n    format: \"%s\"\n", "`format:` must contain exactly one %d or %.Nf"},
		{"format bare %f", "tiles:\n  - kind: counter\n    cron: a\n    format: \"%f\"\n", "`format:` must contain exactly one %d or %.Nf"},
		{"format stray percent", "tiles:\n  - kind: counter\n    cron: a\n    format: \"%d %\"\n", "`format:` must contain exactly one %d or %.Nf"},
		{"max on counter", "tiles:\n  - kind: counter\n    cron: a\n    max: 5\n", "`max:` is only supported on gauge"},
		{"bucket on counter", "tiles:\n  - kind: counter\n    cron: a\n    bucket: 5m\n", "`bucket:` is only supported on line, bar and sparkline"},
		{"bad range", "tiles:\n  - kind: line\n    cron: a\n    range: sometime\n", "invalid range"},
		{"bucket on table", "tiles:\n  - kind: table\n    cron: a\n    bucket: 5m\n", "`bucket:` is only supported on line, bar and sparkline"},
		{"unknown aggregate", "tiles:\n  - kind: line\n    cron: a\n    aggregate: p99\n", "unknown aggregate \"p99\""},
		{"identity is pipe-only", "tiles:\n  - kind: line\n    cron: a\n    aggregate: identity\n", "unknown aggregate"},
		{"bad bucket", "tiles:\n  - kind: bar\n    cron: a\n    bucket: soon\n", "invalid bucket"},
		{"zero bucket", "tiles:\n  - kind: bar\n    cron: a\n    bucket: 0s\n", "invalid bucket"},
		{"tile tag points at the cron's schema", "tiles:\n  - kind: sparkline\n    cron: a\n    tag: .host\n", "a tile has no 'tag' key: a sparkline draws one line per tag its cron stores"},
		{"tile y points at the cron's schema", "tiles:\n  - kind: sparkline\n    cron: a\n    y: .v\n", "'# value:' and '# tag:' headers"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseDashboardYAML(c.body)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}
}

func TestParseDashboardYAML_KindFieldsAccepted(t *testing.T) {
	body := `
tiles:
  - kind: counter
    cron: a
    format: "$%.2f"
  - kind: counter
    cron: e
    format: "%d%%"
  - kind: gauge
    cron: b
    max: 50
  - kind: sparkline
    cron: c
  - kind: line
    cron: f
    aggregate: p95
    bucket: 1h
    range: 7d..
    title: Latency
  - kind: bar
    cron: g
    aggregate: count
    bucket: 90s
  - kind: counter
    cron: h
    aggregate: max
    range: 1h..
  - kind: gauge
    cron: i
    range: 7d..
  - kind: table
    cron: d
    aggregate: p95
    range: 2h..
  - kind: table
    cron: d
`
	if _, err := ParseDashboardYAML(body); err != nil {
		t.Fatalf("valid per-kind fields rejected: %v", err)
	}
}

func TestParseDashboardYAML_DefaultsApplied(t *testing.T) {
	body := `
tiles:
  - kind: sparkline
    cron: foo
`
	spec, err := ParseDashboardYAML(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tile := spec.Sections[0].Tiles[0]
	if tile.Size != "" {
		t.Errorf("size should be empty (renderer applies default), got %q", tile.Size)
	}
}

// The tile key is `cron:`. A `watch:` key gets an error that names `cron:`.
func TestParseDashboardYAML_WatchKeyNamesCron(t *testing.T) {
	_, err := ParseDashboardYAML("tiles:\n  - kind: counter\n    watch: errs\n")
	if err == nil || !strings.Contains(err.Error(), "saw 'watch', expected 'cron'") {
		t.Fatalf("watch: key: err = %v, want \"saw 'watch', expected 'cron'\"", err)
	}
	spec, err := ParseDashboardYAML("tiles:\n  - kind: counter\n    cron: errs\n")
	if err != nil {
		t.Fatalf("cron: key: %v", err)
	}
	if got := spec.Sections[0].Tiles[0].Watch; got != "errs" {
		t.Errorf("tile cron = %q, want errs", got)
	}
	if _, err := ParseDashboardYAML("tiles:\n  - kind: counter\n"); err == nil || !strings.Contains(err.Error(), "missing `cron:`") {
		t.Errorf("missing key: err = %v", err)
	}
}

func TestFormatTileDisplay(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	cases := []struct {
		name string
		tile DashboardTile
		v    *float64
		want *TileDisplay
	}{
		{"int verb rounds", DashboardTile{Kind: "counter", Format: "%d"}, f(41.6), &TileDisplay{Num: "42"}},
		{"suffix", DashboardTile{Kind: "counter", Format: "%d ms"}, f(3), &TileDisplay{Num: "3", After: " ms"}},
		{"prefix and decimals", DashboardTile{Kind: "counter", Format: "$%.1f"}, f(2.56), &TileDisplay{Before: "$", Num: "2.6"}},
		{"literal percent", DashboardTile{Kind: "counter", Format: "%d%%"}, f(99.5), &TileDisplay{Num: "100", After: "%"}},
		{"percent before verb", DashboardTile{Kind: "counter", Format: "%%%d"}, f(7), &TileDisplay{Before: "%", Num: "7"}},
		{"no format whole", DashboardTile{Kind: "counter"}, f(3), &TileDisplay{Num: "3"}},
		{"no format fraction", DashboardTile{Kind: "counter"}, f(3.5), &TileDisplay{Num: "3.50"}},
		{"no data", DashboardTile{Kind: "counter", Format: "%d ms"}, nil, &TileDisplay{Num: "—"}},
		{"gauge default max", DashboardTile{Kind: "gauge"}, f(50), &TileDisplay{Num: "50", After: "/100"}},
		{"gauge max", DashboardTile{Kind: "gauge", Max: f(200)}, f(50), &TileDisplay{Num: "50", After: "/200"}},
		{"gauge no data", DashboardTile{Kind: "gauge"}, nil, &TileDisplay{Num: "—", After: "/100"}},
		{"sparkline has none", DashboardTile{Kind: "sparkline"}, f(1), nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FormatTileDisplay(c.tile, c.v)
			if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

// A tile is `chart <cron>` with the same options: its ChartArgs are what the
// shell would parse from the equivalent command line.
func TestDashboardTile_ChartArgsMatchTheChartCommand(t *testing.T) {
	spec, err := ParseDashboardYAML("tiles:\n  - kind: bar\n    cron: errs\n    aggregate: max\n    bucket: 1h\n    title: Worst\n  - kind: line\n    cron: plain\n")
	if err != nil {
		t.Fatal(err)
	}
	tiles := spec.Sections[0].Tiles
	for i, cmd := range [][]string{
		{"errs", "--kind", "bar", "-a", "max", "--bucket", "1h", "--title", "Worst"},
		{"plain", "--kind", "line"},
	} {
		want, msg := chart.ParseWatchCmd(cmd)
		if msg != "" {
			t.Fatal(msg)
		}
		got := tiles[i].ChartArgs()
		want.Interval, want.Follow = 0, false // watching is a command-line concern
		if got != want {
			t.Errorf("tile %d: ChartArgs() = %+v, chart %v parses to %+v", i, got, cmd, want)
		}
	}
}
