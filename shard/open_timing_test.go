package shard

import (
	"path/filepath"
	"strings"
	"testing"
)

// A spare opens its database inside an upgrade's hold window, so the open
// reports where its time went: which of its steps a slow open spent it in.
func TestNewDuckDBShard_ReportsWhereTheOpenSpentItsTime(t *testing.T) {
	ds, err := NewDuckDBShard("s-1", filepath.Join(t.TempDir(), "shard.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	got := ds.OpenTiming()
	for _, step := range []string{"connect=", "create=", "migrate=", "count="} {
		if !strings.Contains(got, step) {
			t.Errorf("OpenTiming() = %q, want it to name %q", got, step)
		}
	}
}
