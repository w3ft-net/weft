package shard

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shape"
)

func TestLoadIdentity_LegacyVolumeAdoptsCustomerLayout(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"shard_id":"s1","window_start":"2026-04-13T00:00:00Z","window_end":"2026-04-20T00:00:00Z","apps":["weft"]}`
	if err := os.WriteFile(filepath.Join(dir, "shard.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := id.Layout, shape.Legacy(); !reflect.DeepEqual(got, want) {
		t.Fatalf("layout = %+v, want legacy %+v", got, want)
	}
}

// A layout with no partition columns is a real layout (the open-source shape
// has no tenant), not an absent one: it must survive a reload and a trip over
// the wire instead of being adopted as the legacy customer_id layout.
func TestEmptyLayoutIsNotAbsent(t *testing.T) {
	empty := shape.NewLayout()
	dir := t.TempDir()
	if err := SaveIdentity(dir, &Identity{ShardID: "s1", Layout: empty}); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(id.Layout.Fingerprint, empty.Fingerprint) || len(id.Layout.Columns) != 0 {
		t.Errorf("reloaded layout = %+v, want the empty layout %+v", id.Layout, empty)
	}
	if got := LayoutFromProto(LayoutToProto(empty)); got.Fingerprint != empty.Fingerprint || len(got.Columns) != 0 {
		t.Errorf("wire round trip = %+v, want the empty layout", got)
	}
	if got := LayoutFromProto(nil); !got.IsLegacy() {
		t.Errorf("absent wire layout = %+v, want legacy", got)
	}
}

func TestSaveLoadIdentity_LayoutRoundtrip(t *testing.T) {
	dir := t.TempDir()
	lay := shape.NewLayout(
		shape.LayoutColumn{Name: "region", Type: shape.TypeString, Role: shape.RolePartition},
	)
	if err := SaveIdentity(dir, &Identity{ShardID: "s1", Layout: lay}); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(id.Layout, lay) {
		t.Fatalf("layout = %+v, want %+v", id.Layout, lay)
	}
}

func TestLoadIdentity_Missing(t *testing.T) {
	dir := t.TempDir()

	id, err := LoadIdentity(dir)
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if id != nil {
		t.Fatalf("expected nil identity for missing file, got %+v", id)
	}
}

func TestSaveLoadIdentity_Roundtrip(t *testing.T) {
	dir := t.TempDir()

	want := &Identity{
		ShardID:     "shard-2026w16-small-01",
		WindowStart: time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC),
		Apps:        []string{"weft", "vimgym"},
	}

	if err := SaveIdentity(dir, want); err != nil {
		t.Fatal(err)
	}

	got, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected identity, got nil")
	}
	if got.ShardID != want.ShardID {
		t.Errorf("ShardID: got %q want %q", got.ShardID, want.ShardID)
	}
	if !got.WindowStart.Equal(want.WindowStart) {
		t.Errorf("WindowStart: got %v want %v", got.WindowStart, want.WindowStart)
	}
	if !got.WindowEnd.Equal(want.WindowEnd) {
		t.Errorf("WindowEnd: got %v want %v", got.WindowEnd, want.WindowEnd)
	}
	if len(got.Apps) != len(want.Apps) {
		t.Fatalf("Apps length: got %d want %d", len(got.Apps), len(want.Apps))
	}
	for i, a := range want.Apps {
		if got.Apps[i] != a {
			t.Errorf("Apps[%d]: got %q want %q", i, got.Apps[i], a)
		}
	}
}

// SaveIdentity should be atomic — a partial write left behind by a crash
// must not be loadable as a valid identity. Simulate by pre-seeding an
// incomplete file and confirming a subsequent Save replaces it cleanly.
func TestSaveIdentity_AtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shard.json")

	// Seed an incomplete file.
	if err := os.WriteFile(path, []byte(`{"shard_id":"half-writ`), 0644); err != nil {
		t.Fatal(err)
	}

	id := &Identity{
		ShardID:     "shard-complete",
		WindowStart: time.Unix(1000, 0).UTC(),
		WindowEnd:   time.Unix(2000, 0).UTC(),
	}
	if err := SaveIdentity(dir, id); err != nil {
		t.Fatal(err)
	}

	got, err := LoadIdentity(dir)
	if err != nil {
		t.Fatalf("after atomic save, load should succeed: %v", err)
	}
	if got.ShardID != "shard-complete" {
		t.Errorf("ShardID: got %q", got.ShardID)
	}
}
