// Command compute is the single-machine compute worker. It reads archives with
// DuckDB in its own process, so it is a cgo build and needs no object store.
package main

import (
	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/cmd/compute/server"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/shard"
	_ "github.com/w3ft-net/weft/shard/duckdbdrv"
)

func main() {
	server.Run(server.Options{
		Readers: func() map[string]archive.Reader {
			return map[string]archive.Reader{compute.ArchiveReaderLocal: shard.LocalReader(shard.S3ConfigFromEnv())}
		},
		DefaultReader: compute.ArchiveReaderLocal,
	})
}
