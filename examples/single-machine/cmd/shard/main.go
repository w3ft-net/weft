// Command shard is the single-machine shard node: DuckDB on the local disk,
// archives exported to a local directory.
package main

import (
	_ "github.com/w3ft-net/weft/shard/duckdbdrv"

	"github.com/w3ft-net/weft/cmd/shard/server"
)

func main() { server.Run(server.Options{}) }
