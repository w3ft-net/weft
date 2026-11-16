package live_test

import (
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/dialect"
	"github.com/w3ft-net/weft/backends/logs/live"
)

type fake struct {
	name string
	d    dialect.Dialect
}

func (f fake) Name() string                             { return f.name }
func (f fake) Dialect() dialect.Dialect                 { return f.d }
func (f fake) Open(live.OpenParams) (live.Store, error) { return nil, nil }

func TestRegistry(t *testing.T) {
	if err := live.Register(fake{"test-store", dialect.DuckDB}); err != nil {
		t.Fatal(err)
	}
	d, err := live.Lookup("test-store")
	if err != nil || d.Name() != "test-store" {
		t.Fatalf("Lookup = %v, %v", d, err)
	}
	if live.Register(fake{"test-store", dialect.DuckDB}) == nil {
		t.Error("a name registered twice")
	}
	if live.Register(fake{"", dialect.DuckDB}) == nil {
		t.Error("a driver with no name")
	}
	if live.Register(fake{"no-dialect", nil}) == nil {
		t.Error("a driver with no dialect")
	}
	if _, err := live.Lookup("nope"); err == nil || !strings.Contains(err.Error(), "test-store") {
		t.Errorf("an unknown name should list what there is: %v", err)
	}
}
