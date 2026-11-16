package shard

import (
	"testing"

	"github.com/w3ft-net/weft/backends/logs/live"
	"github.com/w3ft-net/weft/backends/logs/live/livetest"
)

// DuckDBShard is the open build's live store, and passes what every store does.
func TestDuckDBShard_IsALiveStore(t *testing.T) {
	livetest.Run(t, livetest.Spec{
		New: func(t *testing.T) live.Store {
			ds, err := NewDuckDBShard("conformance", "")
			if err != nil {
				t.Fatal(err)
			}
			return ds
		},
		SelectAll: "SELECT raw, ts FROM logs ORDER BY ts",
	})
}
