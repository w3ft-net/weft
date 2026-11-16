package service

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/w3ft-net/weft/backends/logs/live"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// recordingDriver is DuckDB that notes every open.
type recordingDriver struct {
	live.Driver
	opened []live.OpenParams
}

func (d *recordingDriver) Name() string { return "recording" }

func (d *recordingDriver) Open(p live.OpenParams) (live.Store, error) {
	d.opened = append(d.opened, p)
	return d.Driver.Open(p)
}

func newRecordingDriver() *recordingDriver {
	return &recordingDriver{Driver: shard.DuckDBDriver()}
}

// A server opens its store through the driver it is given, with the shard's
// ID, its path and the layout the coordinator sent, however it comes by the
// identity.
func TestServer_OpensItsStoreThroughTheDriver(t *testing.T) {
	t.Run("on Configure", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "shard.db")
		drv := newRecordingDriver()
		srv := NewBlankServer(dir, path, WithDriver(drv))
		if _, err := srv.Configure(context.Background(), &pb.ConfigureRequest{ShardId: "s-1", Services: []string{"x"}}); err != nil {
			t.Fatal(err)
		}
		defer srv.Shard().Close()
		if len(drv.opened) != 1 || drv.opened[0].ID != "s-1" || drv.opened[0].Path != path {
			t.Errorf("opened = %+v", drv.opened)
		}
		if srv.Driver() != live.Driver(drv) {
			t.Error("the server does not report its driver")
		}
	})
	t.Run("with no persistence (in-process)", func(t *testing.T) {
		drv := newRecordingDriver()
		srv := NewBlankServer("", "", WithDriver(drv))
		if _, err := srv.Configure(context.Background(), &pb.ConfigureRequest{ShardId: "s-2"}); err != nil {
			t.Fatal(err)
		}
		defer srv.Shard().Close()
		if len(drv.opened) != 1 || drv.opened[0].ID != "s-2" || drv.opened[0].Path != "" {
			t.Errorf("opened = %+v", drv.opened)
		}
	})
	t.Run("on restart from an identity", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "shard.db")
		first := NewBlankServer(dir, path)
		if _, err := first.Configure(context.Background(), &pb.ConfigureRequest{ShardId: "s-3"}); err != nil {
			t.Fatal(err)
		}
		first.Shard().Close()

		drv := newRecordingDriver()
		srv, err := NewServerFromIdentity(dir, path, WithDriver(drv))
		if err != nil {
			t.Fatal(err)
		}
		defer srv.Shard().Close()
		if len(drv.opened) != 1 || drv.opened[0].ID != "s-3" {
			t.Errorf("opened = %+v", drv.opened)
		}
	})
}

func TestServer_DefaultsToDuckDB(t *testing.T) {
	if got := NewBlankServer("", "").Driver().Name(); got != "duckdb" {
		t.Errorf("default driver = %q", got)
	}
}
