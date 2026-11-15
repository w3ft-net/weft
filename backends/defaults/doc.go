// Package defaults is the open build's backends: one file per domain, each the
// implementation a build gets when it supplies nothing else. Most are SQLite
// over the one database sqlitedb opens (they take its *sql.DB, so they share
// it); the dead-letter store is in memory. Stores opens them all over one
// database. A hosted build supplies its
// own and does not link these. The suites each store passes live beside its contract,
// in <domain>/<domain>storetest.
//
// The package must load with CGO_ENABLED=0, and each default must be cheap to
// link: a store that needs a heavy dependency is its own package instead.
package defaults
