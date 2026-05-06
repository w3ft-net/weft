package store

import (
	"strings"
	"testing"
)

func TestParseDashboardYAML_Sections(t *testing.T) {
	body := `
title: Production
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        watch: error-rate-hourly
        size: md
      - kind: table
        watch: top-errors-hourly
        columns: [host, count]
  - title: Capacity
    tiles:
      - kind: counter
        watch: pending-jobs
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
    watch: pending-jobs
  - kind: sparkline
    watch: error-rate
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
		{"unknown kind", "tiles:\n  - kind: pizza\n    watch: foo\n", "unknown kind"},
		{"missing watch", "tiles:\n  - kind: sparkline\n", "missing `watch:`"},
		{"missing kind", "tiles:\n  - watch: foo\n", "missing `kind:`"},
		{"bad size", "tiles:\n  - kind: sparkline\n    watch: foo\n    size: huge\n", "unknown size"},
		{"table no columns", "tiles:\n  - kind: table\n    watch: foo\n", "table tile requires `columns:`"},
		{"sections + flat tiles", "tiles:\n  - kind: counter\n    watch: foo\nsections:\n  - tiles:\n      - kind: counter\n        watch: bar\n", "cannot mix"},
		{"empty section", "sections:\n  - title: foo\n    tiles: []\n", "no tiles"},
		{"unknown field at top", "title: x\nfoo: bar\ntiles:\n  - kind: counter\n    watch: a\n", "not found"},
		{"unknown field in section", "sections:\n  - heading: Ops\n    tiles:\n      - kind: counter\n        watch: a\n", "not found"},
		{"unknown field in tile", "tiles:\n  - kind: counter\n    watch: a\n    color: red\n", "not found"},
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

func TestParseDashboardYAML_DefaultsApplied(t *testing.T) {
	body := `
tiles:
  - kind: sparkline
    watch: foo
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
