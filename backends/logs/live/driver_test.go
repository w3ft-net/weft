package live_test

import (
	"fmt"
	"strings"
	"sync/atomic"
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

// runs numbers the test's registrations: the registry is the process's, so a
// test run again in the same process (-count) must not register a name twice.
var runs atomic.Int64

func TestRegistry(t *testing.T) {
	name := fmt.Sprintf("test-store-%d", runs.Add(1))
	if err := live.Register(fake{name, dialect.DuckDB}); err != nil {
		t.Fatal(err)
	}
	d, err := live.Lookup(name)
	if err != nil || d.Name() != name {
		t.Fatalf("Lookup = %v, %v", d, err)
	}
	if live.Register(fake{name, dialect.DuckDB}) == nil {
		t.Error("a name registered twice")
	}
	if live.Register(fake{"", dialect.DuckDB}) == nil {
		t.Error("a driver with no name")
	}
	if live.Register(fake{"no-dialect", nil}) == nil {
		t.Error("a driver with no dialect")
	}
	if _, err := live.Lookup("nope"); err == nil || !strings.Contains(err.Error(), name) {
		t.Errorf("an unknown name should list what there is: %v", err)
	}
}
