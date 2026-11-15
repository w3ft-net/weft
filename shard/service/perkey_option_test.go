package service

import (
	"context"
	"path/filepath"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// A server built with shard.WithPerKeyTables opens every database it creates
// per-key, whichever way it comes to open it: christened by Configure, reopened
// from an identity on disk, or activated as a spare. docs/plans/per-service-tables.md.

func TestServer_ConfigureOpensPerKeyWhenAsked(t *testing.T) {
	for _, c := range []struct {
		name string
		opts []shard.Option
		want bool
	}{
		{"with the option", []shard.Option{shard.WithPerKeyTables()}, true},
		{"without it", nil, false},
	} {
		dir := t.TempDir()
		srv := NewBlankServer(dir, filepath.Join(dir, "shard.duckdb"), c.opts...)
		if _, err := srv.Configure(context.Background(), &pb.ConfigureRequest{ShardId: "opt-test"}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		ds, err := srv.getShard()
		if err != nil {
			t.Fatal(err)
		}
		if ds.PerKey() != c.want {
			t.Errorf("%s: PerKey = %v, want %v", c.name, ds.PerKey(), c.want)
		}
		ds.Close()
	}
}

// A shard that was christened and restarted before it took a row is still empty
// when it reopens; it must come back per-key.
func TestServer_ReopenFromIdentityOpensPerKeyWhenAsked(t *testing.T) {
	dir := t.TempDir()
	if err := shard.SaveIdentity(dir, &shard.Identity{ShardID: "reopen-test"}); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServerFromIdentity(dir, filepath.Join(dir, "shard.duckdb"), shard.WithPerKeyTables())
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shard().Close()
	if !srv.Shard().PerKey() {
		t.Error("an empty database reopened from its identity is not per-key")
	}
}

// A spare opens the database on the volume it is given once it is activated.
func TestServer_OpenFromDiskOpensPerKeyWhenAsked(t *testing.T) {
	dir := t.TempDir()
	if err := shard.SaveIdentity(dir, &shard.Identity{ShardID: "spare-test"}); err != nil {
		t.Fatal(err)
	}
	srv := NewSpareServer(dir, filepath.Join(dir, "shard.duckdb"), nil, shard.WithPerKeyTables())
	if err := srv.openFromDisk(); err != nil {
		t.Fatal(err)
	}
	defer srv.Shard().Close()
	if !srv.Shard().PerKey() {
		t.Error("a spare's database is not per-key")
	}
}
