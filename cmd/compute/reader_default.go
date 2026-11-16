//go:build !localreader

package main

// defaultArchiveReader is the reader used when no -archive-reader flag names
// one. This build links no DuckDB, so it has none: the Lambda reader must be
// chosen.
const defaultArchiveReader = ""
