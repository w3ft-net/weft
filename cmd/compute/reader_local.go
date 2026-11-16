//go:build localreader

package main

import (
	"github.com/w3ft-net/weft/compute"
	_ "github.com/w3ft-net/weft/shard/duckdbdrv"
)

// defaultArchiveReader is the in-process DuckDB reader: this build links DuckDB,
// so archives are readable with no Lambda. Cold reads run in the compute
// process and take its CPU and memory.
const defaultArchiveReader = compute.ArchiveReaderLocal
