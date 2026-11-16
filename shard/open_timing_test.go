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
	for _, step := range []string{"open=", "create=", "migrate=", "count="} {
		if !strings.Contains(got, step) {
			t.Errorf("OpenTiming() = %q, want it to name %q", got, step)
		}
	}
}

// The driver opens the database file inside sql.Open, so the step that names
// the file open has to include it: a "connect" measured after it read 0s on
// opens that took 5s in prod.
func TestNewDuckDBShard_TheOpenStepIncludesOpeningTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shard.db")
	first, err := NewDuckDBShard("s-1", path)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()

	ds, err := NewDuckDBShard("s-1", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ds.Close() })

	got := ds.OpenTiming()
	if !strings.Contains(got, "open=") || strings.Contains(got, "open=0s") {
		t.Errorf("OpenTiming() = %q, want an open= step that measured the file open", got)
	}
}
