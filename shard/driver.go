package shard

import (
	"errors"

	"github.com/w3ft-net/weft/backends/logs/dialect"
	"github.com/w3ft-net/weft/backends/logs/live"
)

// DuckDBDriver is the live store of the open build: a DuckDB database in one
// file, or in memory where there is no path. opts configure every database it
// opens (WithPerKeyTables); the layout is the node's, given at Open.
func DuckDBDriver(opts ...Option) live.Driver { return duckDriver{opts: opts} }

type duckDriver struct{ opts []Option }

func (duckDriver) Name() string { return "duckdb" }

func (duckDriver) Dialect() dialect.Dialect { return dialect.DuckDB }

func (d duckDriver) Open(p live.OpenParams) (live.Store, error) {
	// A copy, so the driver's options are never appended into twice.
	opts := append(d.opts[:len(d.opts):len(d.opts)], WithLayout(p.Layout))
	ds, err := NewDuckDBShard(p.ID, p.Path, opts...)
	if err != nil {
		return nil, err // not ds: a nil *DuckDBShard in a live.Store is not nil
	}
	return ds, nil
}

// errNoDuckDB is what opening a DuckDB database says in a binary that did not
// link the driver.
var errNoDuckDB = errors.New("this binary does not link DuckDB: import github.com/w3ft-net/weft/shard/duckdbdrv (weft-compute: build with -tags localreader)")
