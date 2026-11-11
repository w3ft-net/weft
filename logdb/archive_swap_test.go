package logdb

import (
	"errors"
	"testing"
)

// SwapArchivePath moves a shard's archive from one path to another, the last step
// of converting it by hand (docs/plans/per-service-tables.md, "Archive layout"):
// only if the shard is still at the old path, with the old objects tombstoned so
// they are deleted after a grace and a rollback is possible until then.

const (
	swapOld = "s3://b/40-apps/shard-1.attempt-1/m1440/**/*.parquet"
	swapNew = "s3://b/40-apps/shard-1.attempt-1.r9/sized/**/*.parquet"
)

func tombstonesFor(t *testing.T, ss ShardStore, id string) []ParquetTombstone {
	t.Helper()
	var tombs []ParquetTombstone
	if err := ss.DB().Where("shard_id = ?", id).Find(&tombs).Error; err != nil {
		t.Fatal(err)
	}
	return tombs
}

func TestSwapArchivePath_MovesTheArchiveAndTombstonesTheOldOne(t *testing.T) {
	ss := archivedShard(t, "sh-swap", swapOld)
	if err := ss.SwapArchivePath("sh-swap", swapOld, swapNew, swapNow, ArchiveSwapGCDelay); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := ss.GetShard("sh-swap")
	if rec.ParquetPath != swapNew {
		t.Errorf("parquet_path = %q, want %q", rec.ParquetPath, swapNew)
	}
	tombs := tombstonesFor(t, ss, "sh-swap")
	if len(tombs) != 1 || tombs[0].Path != swapOld || tombs[0].Reason != "swap" {
		t.Fatalf("tombstones = %+v, want one for the old path with reason swap", tombs)
	}
	if want := swapNow.Add(ArchiveSwapGCDelay); !tombs[0].DeleteAfter.Equal(want) {
		t.Errorf("delete_after = %v, want %v", tombs[0].DeleteAfter, want)
	}
}

// A shard whose archive is somewhere else (a retention pass rewrote it, or the
// caller has the wrong shard) is left alone, and nothing is tombstoned.
func TestSwapArchivePath_RefusesWhenTheShardIsNotAtTheOldPath(t *testing.T) {
	ss := archivedShard(t, "sh-elsewhere", "s3://b/other.parquet")
	err := ss.SwapArchivePath("sh-elsewhere", swapOld, swapNew, swapNow, ArchiveSwapGCDelay)
	if !errors.Is(err, ErrArchivePathChanged) {
		t.Fatalf("err = %v, want ErrArchivePathChanged", err)
	}
	rec, _, _ := ss.GetShard("sh-elsewhere")
	if rec.ParquetPath != "s3://b/other.parquet" {
		t.Errorf("parquet_path moved to %q", rec.ParquetPath)
	}
	if n := len(tombstonesFor(t, ss, "sh-elsewhere")); n != 0 {
		t.Errorf("%d tombstones recorded for a refused swap", n)
	}
}

// Every coordinator is told to swap, and only the first finds the old path. The
// rest find the new one and must succeed without tombstoning anything twice.
func TestSwapArchivePath_IsIdempotent(t *testing.T) {
	ss := archivedShard(t, "sh-twice", swapOld)
	for i := 0; i < 2; i++ {
		if err := ss.SwapArchivePath("sh-twice", swapOld, swapNew, swapNow, ArchiveSwapGCDelay); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	if n := len(tombstonesFor(t, ss, "sh-twice")); n != 1 {
		t.Errorf("%d tombstones after two swaps, want 1", n)
	}
}

func TestSwapArchivePath_UnknownShard(t *testing.T) {
	ss := archivedShard(t, "sh-known", swapOld)
	err := ss.SwapArchivePath("no-such-shard", swapOld, swapNew, swapNow, ArchiveSwapGCDelay)
	if err == nil || errors.Is(err, ErrArchivePathChanged) {
		t.Fatalf("err = %v, want a not-found error", err)
	}
}

// swapProvisioner is a provisioner over the fixture's store whose registry holds
// the shard as it is in the database.
func swapProvisioner(t *testing.T, id string) (*Provisioner, ShardStore) {
	t.Helper()
	ss := archivedShard(t, id, swapOld)
	reg := NewShardRegistry()
	p := NewProvisioner(reg, nil, ProvisionerConfig{})
	p.SetStore(ss.Store)
	rec, _, _ := ss.GetShard(id)
	reg.Add(rec)
	return p, ss
}

// The provisioner's side: the live registry follows the database, so queries
// read the new archive without a restart.
func TestProvisionerSwapArchive_MovesTheLiveEntryToo(t *testing.T) {
	p, ss := swapProvisioner(t, "sh-prov")
	if err := p.SwapArchive("sh-prov", swapOld, swapNew); err != nil {
		t.Fatal(err)
	}
	if got := p.registry.Get("sh-prov").ParquetPath; got != swapNew {
		t.Errorf("registry entry is at %q, want %q", got, swapNew)
	}
	rec, _, _ := ss.GetShard("sh-prov")
	if rec.ParquetPath != swapNew {
		t.Errorf("database is at %q, want %q", rec.ParquetPath, swapNew)
	}
	// the other coordinator, told the same, finds the database already moved and
	// brings its own registry along
	if err := p.SwapArchive("sh-prov", swapOld, swapNew); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

func TestProvisionerSwapArchive_OnlyAnArchivedShard(t *testing.T) {
	p, _ := swapProvisioner(t, "sh-live")
	e := p.registry.Get("sh-live")
	e.mu.Lock()
	e.State = StateIngesting
	e.mu.Unlock()
	err := p.SwapArchive("sh-live", swapOld, swapNew)
	if !errors.Is(err, ErrShardWrongState) {
		t.Fatalf("err = %v, want ErrShardWrongState", err)
	}
	if got := p.registry.Get("sh-live").ParquetPath; got != swapOld {
		t.Errorf("a refused swap moved the registry entry to %q", got)
	}
}

func TestProvisionerSwapArchive_UnknownShard(t *testing.T) {
	p, _ := swapProvisioner(t, "sh-here")
	if err := p.SwapArchive("sh-gone", swapOld, swapNew); !errors.Is(err, ErrShardNotInRegistry) {
		t.Fatalf("err = %v, want ErrShardNotInRegistry", err)
	}
}

func TestProvisionerSwapArchive_WrongOldPathChangesNothing(t *testing.T) {
	p, ss := swapProvisioner(t, "sh-wrong")
	err := p.SwapArchive("sh-wrong", "s3://b/not-the-archive.parquet", swapNew)
	if !errors.Is(err, ErrArchivePathChanged) {
		t.Fatalf("err = %v, want ErrArchivePathChanged", err)
	}
	if got := p.registry.Get("sh-wrong").ParquetPath; got != swapOld {
		t.Errorf("registry entry moved to %q", got)
	}
	if n := len(tombstonesFor(t, ss, "sh-wrong")); n != 0 {
		t.Errorf("%d tombstones for a refused swap", n)
	}
}
