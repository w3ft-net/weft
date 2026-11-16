package heartbeat

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/w3ft-net/weft/backends/logs/dialect"
	"github.com/w3ft-net/weft/backends/logs/live"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shard"
	shardsvc "github.com/w3ft-net/weft/shard/service"
)

// noVolume is a Volume that is always attached: activation mounts nothing.
type noVolume struct{}

func (noVolume) Mount(context.Context) error   { return nil }
func (noVolume) Unmount(context.Context) error { return nil }

// recordingCoord captures the heartbeat request for assertion.
type recordingCoord struct {
	pb.UnimplementedWeftCoordinatorServer
	last *pb.ShardHeartbeatRequest
}

func (r *recordingCoord) Heartbeat(_ context.Context, req *pb.ShardHeartbeatRequest) (*pb.HeartbeatResponse, error) {
	r.last = req
	return &pb.HeartbeatResponse{}, nil
}

// The shard must include its EC2 instance id in every heartbeat.
// christen() looks up the pre-attached volume via
// tag:shard_id=pending-<instance_id>; without this field the lookup
// silently fails and the volume's shard_id tag never gets rewritten
// from pending-... to the real shard id. Observed 2026-04-18 in prod
// as a `volume=` empty string on the christening log line.
func TestIncludesInstanceID(t *testing.T) {
	req := Build("shard-x", "10.0.0.1:9000", "i-abc123", "INGESTING", "", 0, 4096)

	if req.InstanceId != "i-abc123" {
		t.Errorf("InstanceId: got %q want i-abc123", req.InstanceId)
	}
	if req.ShardId != "shard-x" {
		t.Errorf("ShardId: got %q want shard-x", req.ShardId)
	}
	if req.DiskBytes != 4096 {
		t.Errorf("DiskBytes: got %d want 4096", req.DiskBytes)
	}
}

// Unclaimed shards (no shard.json yet) still heartbeat — they carry
// empty ShardId but MUST carry their instance id so the coordinator
// can find the pre-attached volume at christen time.
func TestUnclaimedStillCarriesInstanceID(t *testing.T) {
	req := Build("", "10.0.0.1:9000", "i-fresh", "ADOPTED", "", 0, 0)

	if req.InstanceId != "i-fresh" {
		t.Errorf("InstanceId: got %q want i-fresh", req.InstanceId)
	}
	if req.ShardId != "" {
		t.Errorf("ShardId: got %q want empty", req.ShardId)
	}
}

// A warm-pool spare heartbeats as one, with no shard id, so the
// coordinator refreshes the pool's record of it and never correlates it to
// a LAUNCHING shard.
func TestSpareAnnouncesItself(t *testing.T) {
	srv := shardsvc.NewSpareServer(t.TempDir(), "", noVolume{})
	req, send := For(srv, "10.0.0.2:9000", "i-spare", "")
	if !send {
		t.Fatal("a spare must heartbeat")
	}
	if !req.Spare || req.ShardId != "" || req.InstanceId != "i-spare" {
		t.Fatalf("spare heartbeat: spare=%v shard=%q instance=%q", req.Spare, req.ShardId, req.InstanceId)
	}
}

// A configured shard heartbeats the layout its volume records, so the
// coordinator can refuse one it would misread. One with no shard yet has none
// to report.
func TestReportsTheVolumesLayout(t *testing.T) {
	blank := shardsvc.NewBlankServer(t.TempDir(), "")
	if req, _ := For(blank, "10.0.0.2:9000", "i-new", ""); req.LayoutFingerprint != "" {
		t.Fatalf("an unconfigured shard reported layout %q", req.LayoutFingerprint)
	}

	lay := shape.NewLayout(shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition})
	dir := t.TempDir()
	srv := shardsvc.NewBlankServer(dir, filepath.Join(dir, "shard.duckdb"))
	if _, err := srv.Configure(context.Background(), &pb.ConfigureRequest{ShardId: "s1", Layout: shard.LayoutToProto(lay)}); err != nil {
		t.Fatal(err)
	}
	req, send := For(srv, "10.0.0.2:9000", "i-s1", "")
	if !send {
		t.Fatal("a configured shard must heartbeat")
	}
	if req.LayoutFingerprint != lay.Fingerprint {
		t.Fatalf("heartbeat layout = %q, want %q", req.LayoutFingerprint, lay.Fingerprint)
	}
}

// A paused shard has handed its volume to a successor; a beat from the old
// address would pull callers back to it, so it stays silent.
func TestPausedShardIsSilent(t *testing.T) {
	srv := shardsvc.NewBlankServer(t.TempDir(), "")
	if _, err := srv.Pause(context.Background(), &pb.PauseRequest{SuccessorAddr: "10.0.0.3:9000"}); err != nil {
		t.Fatal(err)
	}
	if _, send := For(srv, "10.0.0.2:9000", "i-old", ""); send {
		t.Fatal("a paused shard must not heartbeat")
	}
}

// bearerSpy records the authorization metadata of the heartbeat it receives.
type bearerSpy struct {
	pb.WeftCoordinatorClient
	got []string
}

func (b *bearerSpy) Heartbeat(ctx context.Context, _ *pb.ShardHeartbeatRequest, _ ...grpc.CallOption) (*pb.HeartbeatResponse, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	b.got = md.Get("authorization")
	return &pb.HeartbeatResponse{}, nil
}

// A shard that has not been christened has no token of its own yet; it
// proves it belongs to the fleet with the shard secret every shard boots with.
func TestSend_UnchristenedShardPresentsTheFleetSecret(t *testing.T) {
	srv := shardsvc.NewBlankServer(t.TempDir(), "")
	spy := &bearerSpy{}
	if !Send(spy, "10.0.0.2:9000", "i-fresh", "", srv, "fleet-secret") {
		t.Fatal("heartbeat not sent")
	}
	if len(spy.got) != 1 || spy.got[0] != "Bearer fleet-secret" {
		t.Errorf("authorization = %v, want [Bearer fleet-secret]", spy.got)
	}
}

func TestFieldFactDeltas_CarryEveryKeyFieldAndTheState(t *testing.T) {
	key := shard.FactKey{Table: "l_7_ns_svc_main_abc", Env: "prod", Locator: "$5"}
	got := FieldFactDeltas([]shard.FieldFact{
		{FactKey: key, Namespace: "ns", Service: "svc", Kind: "main", Customer: 7, State: shard.FactTyped, Epoch: 3},
		{FactKey: key, Namespace: "ns", Service: "svc", Kind: "main", Customer: 7, State: shard.FactDirty, Epoch: 4},
	})
	if len(got) != 2 {
		t.Fatalf("want 2 deltas, got %d", len(got))
	}
	d := got[0]
	if d.Table != "l_7_ns_svc_main_abc" || d.CustomerId != 7 || d.Namespace != "ns" || d.Service != "svc" || d.Kind != "main" || d.Env != "prod" ||
		d.Locator != "$5" || d.Epoch != 3 || d.State != pb.FieldFactState_FIELD_FACT_STATE_TYPED {
		t.Errorf("typed delta = %+v", d)
	}
	if got[1].State != pb.FieldFactState_FIELD_FACT_STATE_DIRTY || got[1].Epoch != 4 {
		t.Errorf("dirty delta = %+v", got[1])
	}
}

// otherDriver is DuckDB under another dialect's name.
type otherDriver struct{ live.Driver }

type otherDialect struct{ dialect.Dialect }

func (otherDialect) Name() string { return "other" }

func (otherDriver) Dialect() dialect.Dialect { return otherDialect{dialect.DuckDB} }

// A shard reports the dialect of the store it runs, so a coordinator planning
// in another can refuse it; a spare and a blank shard report their driver's too.
func TestReportsTheStoresDialect(t *testing.T) {
	for _, tc := range []struct {
		name string
		srv  *shardsvc.Server
		want string
	}{
		{"the default store", shardsvc.NewBlankServer(t.TempDir(), ""), "duckdb"},
		{"another store", shardsvc.NewBlankServer(t.TempDir(), "", shardsvc.WithDriver(otherDriver{shard.DuckDBDriver()})), "other"},
		{"a spare", shardsvc.NewSpareServer(t.TempDir(), "", noVolume{}, shardsvc.WithDriver(otherDriver{shard.DuckDBDriver()})), "other"},
	} {
		req, send := For(tc.srv, "10.0.0.2:9000", "i-x", "")
		if !send {
			t.Fatalf("%s: no heartbeat", tc.name)
		}
		if req.Dialect != tc.want {
			t.Errorf("%s: dialect = %q, want %q", tc.name, req.Dialect, tc.want)
		}
	}
}
