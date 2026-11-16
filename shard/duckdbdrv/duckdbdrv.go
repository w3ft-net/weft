// Package duckdbdrv links the cgo DuckDB driver. Import it for its effect from
// any binary or test that opens DuckDB databases (live shards, local archive
// reads); package shard finds the driver through duckbind.
package duckdbdrv

import (
	"database/sql/driver"

	duckdb "github.com/duckdb/duckdb-go/v2"

	"github.com/w3ft-net/weft/shard/duckbind"
)

func init() {
	duckbind.NewAppender = func(dc driver.Conn, table string, columns []string) (duckbind.Appender, error) {
		a, err := duckdb.NewAppenderWithColumns(dc, "", "", table, columns)
		if err != nil {
			return nil, err // not a: a nil *Appender in an interface is not nil
		}
		return a, nil
	}
	duckbind.UUID = func(id [16]byte) driver.Value { return duckdb.UUID(id) }
}
