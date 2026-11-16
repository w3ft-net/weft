package shard

import (
	"testing"

	"github.com/w3ft-net/weft/backends/logs/live/livetest"
)

// DuckDBShard is the open build's live store, and passes what every store does.
func TestDuckDBDriver_IsALiveStore(t *testing.T) {
	livetest.RunDriver(t, DuckDBDriver(), livetest.Spec{SelectAll: "SELECT raw, ts FROM logs ORDER BY ts"})
}
