package dashboard

import (
	"reflect"
	"testing"
)

func TestNumericTiles(t *testing.T) {
	spec, err := Parse(`
sections:
  - tiles:
      - {kind: sparkline, cron: ops/errors}
      - {kind: table, cron: ops/errors}
      - {kind: table, cron: ops/errors, range: 1h..}
      - {kind: counter, cron: ops/errors}
      - {kind: gauge, cron: ops/other}
`)
	if err != nil {
		t.Fatal(err)
	}
	// A plain table shows text too, so it is not a numeric reader; a windowed one
	// aggregates, which text can't be.
	if got, want := NumericTiles(spec, "ops/errors"), []string{"counter", "sparkline", "table"}; !reflect.DeepEqual(got, want) {
		t.Errorf("kinds on ops/errors = %v, want %v", got, want)
	}
	if got := NumericTiles(spec, "ops/none"); len(got) != 0 {
		t.Errorf("kinds on a cron no tile names = %v", got)
	}
}
