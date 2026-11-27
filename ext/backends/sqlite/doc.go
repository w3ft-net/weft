// Package sqlite is every store contract over one embedded SQLite database: one
// file per domain, each taking the *sql.DB that sqlitedb.Open returns, so they
// share a file, a connection and, where one domain refers to another, real foreign
// keys. Open makes them all over one database. The suites each store passes live
// beside its contract, in <domain>/<domain>storetest.
//
// The driver is modernc.org/sqlite, a pure-Go translation of SQLite, so a process
// that links this stays cgo-free. The package must load with CGO_ENABLED=0.
package sqlite
