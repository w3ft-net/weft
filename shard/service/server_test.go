package service

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
)

// Configure should persist identity to <dataDir>/shard.json so that an
// instance restart against the same volume can reconstruct its identity
// without re-contacting the coordinator.
func TestConfigure_PersistsIdentity(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shard.duckdb")

	srv := NewBlankServer(dir, dbPath)

	req := &pb.ConfigureRequest{
		ShardId:     "shard-2026w16-small-01",
		Services:    []string{"weft"},
		WindowStart: time.Date(2026, 4, 13, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		WindowEnd:   time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}
	if _, err := srv.Configure(context.Background(), req); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	id, err := shard.LoadIdentity(dir)
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	if id == nil {
		t.Fatal("expected identity, got nil")
	}
	if id.ShardID != req.ShardId {
		t.Errorf("ShardID: got %q want %q", id.ShardID, req.ShardId)
	}
	if len(id.Apps) != 1 || id.Apps[0] != "weft" {
		t.Errorf("Services: got %v want [weft]", id.Apps)
	}
	if id.WindowEnd.Sub(id.WindowStart) != 7*24*time.Hour {
		t.Errorf("expected 7d window, got %v", id.WindowEnd.Sub(id.WindowStart))
	}
}

// The coordinator's layout is what the volume records; an older coordinator
// that sends none leaves the legacy customer_id layout.
func TestConfigure_PersistsLayout(t *testing.T) {
	custom := shape.NewLayout(shape.LayoutColumn{Name: "tier", Type: shape.TypeString, Role: shape.RolePartition})
	for _, tc := range []struct {
		name string
		req  *pb.PartitionLayout
		want shape.Layout
	}{
		{"sent", shard.LayoutToProto(custom), custom},
		{"absent", nil, shape.Legacy()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srv := NewBlankServer(dir, filepath.Join(dir, "shard.duckdb"))
			_, err := srv.Configure(context.Background(), &pb.ConfigureRequest{ShardId: "s1", Layout: tc.req})
			if err != nil {
				t.Fatalf("Configure: %v", err)
			}
			id, err := shard.LoadIdentity(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(id.Layout, tc.want) {
				t.Errorf("layout = %+v, want %+v", id.Layout, tc.want)
			}
		})
	}
}

// A shard that comes up with identity already on disk should refuse a
// second Configure — an already-christened shard must not be re-christened.
func TestConfigure_RejectsReconfigureWhenAlreadyIdentified(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shard.duckdb")

	// Pre-seed an identity file, then start the shard from it.
	id := &shard.Identity{
		ShardID:     "shard-already",
		WindowStart: time.Unix(1000, 0).UTC(),
		WindowEnd:   time.Unix(2000, 0).UTC(),
		Apps:        []string{"weft"},
	}
	if err := shard.SaveIdentity(dir, id); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServerFromIdentity(dir, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// Second Configure with a different id should be rejected.
	req := &pb.ConfigureRequest{
		ShardId:     "shard-different",
		WindowStart: time.Unix(3000, 0).UTC().Format(time.RFC3339),
		WindowEnd:   time.Unix(4000, 0).UTC().Format(time.RFC3339),
	}
	if _, err := srv.Configure(context.Background(), req); err == nil {
		t.Fatal("expected Configure to reject reconfigure, got nil error")
	}
}

// A re-Configure with the SAME id must be an idempotent no-op success. The
// coordinator re-dials + re-Configures on every promote/reconnect (e.g. a
// shard that's still configured from before a coordinator restart); if that
// returns an error the promote gate treats the shard as unreachable and it
// wedges out of rotation forever.
func TestConfigure_IdempotentForSameID(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "shard.duckdb")

	id := &shard.Identity{
		ShardID:     "shard-same",
		WindowStart: time.Unix(1000, 0).UTC(),
		WindowEnd:   time.Unix(2000, 0).UTC(),
		Apps:        []string{"weft"},
	}
	if err := shard.SaveIdentity(dir, id); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServerFromIdentity(dir, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	req := &pb.ConfigureRequest{
		ShardId:     "shard-same",
		WindowStart: time.Unix(1000, 0).UTC().Format(time.RFC3339),
		WindowEnd:   time.Unix(2000, 0).UTC().Format(time.RFC3339),
	}
	if _, err := srv.Configure(context.Background(), req); err != nil {
		t.Fatalf("re-Configure with the same id must succeed (idempotent); got %v", err)
	}
}
