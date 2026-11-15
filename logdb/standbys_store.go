package logdb

import "github.com/w3ft-net/weft/backends/shardstore"

// The warm pool's rows live in the shard-state contract; logdb keeps the names.
type (
	Standby      = shardstore.Standby
	StandbyStore = shardstore.StandbyStore
)
