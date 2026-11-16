// Package duckbind is the seam between package shard and the cgo DuckDB driver.
// It is a leaf and links no DuckDB: shard calls the driver's appender through
// it, and package duckdbdrv, which does link DuckDB, fills it in. A binary
// that opens DuckDB databases imports duckdbdrv; one that does not (the
// default weft-compute) stays cgo-free.
package duckbind

import "database/sql/driver"

// Appender streams rows into one table; DuckDB's Appender satisfies it.
type Appender interface {
	AppendRow(args ...driver.Value) error
	Close() error
}

// The driver's entry points, set by duckdbdrv.
var (
	// NewAppender opens an appender on dc for the named columns of table.
	NewAppender func(dc driver.Conn, table string, columns []string) (Appender, error)
	// UUID wraps id as the value DuckDB's appender takes for a UUID column.
	UUID func(id [16]byte) driver.Value
)

// Linked reports whether the DuckDB driver is part of this binary.
func Linked() bool { return NewAppender != nil }
