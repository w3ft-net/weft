package service

import (
	"context"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/shardstore"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// newRegisteredShard builds a Server with one registry entry that has a
// known heartbeat token. Used by every heartbeat-auth test below.
func newRegisteredShard(t *testing.T, shardID, token string) *Server {
	t.Helper()
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}})
	logdb.SeedShard(coord.Provisioner, &logdb.Shard{
		ShardRow: logdb.ShardRow{
			ID:             shardID,
			State:          logdb.StateIngesting,
			Addr:           "10.0.0.1:9000",
			HeartbeatToken: token,
		},
	})
	return &Server{Coord: coord}
}

func heartbeatCtx(token string) context.Context {
	if token == "" {
		return context.Background()
	}
	return metadata.NewIncomingContext(
		context.Background(),
		metadata.Pairs("authorization", "Bearer "+token),
	)
}

func TestHeartbeat_ValidToken_Accepted(t *testing.T) {
	srv := newRegisteredShard(t, "shard-a", "good-token")
	_, err := srv.Heartbeat(heartbeatCtx("good-token"), &pb.ShardHeartbeatRequest{
		ShardId: "shard-a",
		Addr:    "10.0.0.1:9000",
		State:   "INGESTING",
	})
	if err != nil {
		t.Fatalf("valid heartbeat rejected: %v", err)
	}
}

// A shard stores rows in the layout the coordinator told it, and an archive is
// read back in this build's. One that heartbeats a layout this build does not
// have would be reinterpreted, so it is refused. A shard from before layouts
// sends none, and is the legacy layout the hosted build reproduces.
func TestHeartbeat_LayoutFingerprint(t *testing.T) {
	other := shape.NewLayout(shape.LayoutColumn{Name: "account_id", Type: shape.TypeInt64, Role: shape.RolePartition})
	for _, tc := range []struct {
		name        string
		fingerprint string
		want        codes.Code
	}{
		{"none (an older shard)", "", codes.OK},
		{"this build's layout", shape.BuildLayout().Fingerprint, codes.OK},
		{"another layout", other.Fingerprint, codes.FailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRegisteredShard(t, "shard-a", "good-token")
			_, err := srv.Heartbeat(heartbeatCtx("good-token"), &pb.ShardHeartbeatRequest{
				ShardId: "shard-a", Addr: "10.0.0.1:9000", State: "INGESTING", LayoutFingerprint: tc.fingerprint,
			})
			if got := status.Code(err); got != tc.want {
				t.Fatalf("code = %v (%v), want %v", got, err, tc.want)
			}
		})
	}
}

func TestHeartbeat_MissingBearer_Rejected(t *testing.T) {
	srv := newRegisteredShard(t, "shard-a", "good-token")
	_, err := srv.Heartbeat(heartbeatCtx(""), &pb.ShardHeartbeatRequest{
		ShardId: "shard-a",
	})
	if err == nil {
		t.Fatal("expected Unauthenticated, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

func TestHeartbeat_BadBearer_Rejected(t *testing.T) {
	srv := newRegisteredShard(t, "shard-a", "good-token")
	_, err := srv.Heartbeat(heartbeatCtx("evil-token"), &pb.ShardHeartbeatRequest{
		ShardId: "shard-a",
	})
	if err == nil {
		t.Fatal("expected Unauthenticated, got nil")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

// TestHeartbeat_EmptyShardID_Christen verifies the christen path: a fresh
// shard with no identity yet cannot present a per-shard token, so it presents
// the fleet secret every shard boots with. The coordinator's provisioner is
// what mints the identity + token in response.
func TestHeartbeat_EmptyShardID_Christen(t *testing.T) {
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}})
	srv := &Server{Coord: coord, ShardSecret: "fleet-secret"}

	_, err := srv.Heartbeat(heartbeatCtx("fleet-secret"), &pb.ShardHeartbeatRequest{
		ShardId:    "",
		Addr:       "10.0.0.99:9000",
		InstanceId: "i-fresh",
		State:      "ADOPTED",
	})
	if err != nil {
		t.Fatalf("empty-ID christen heartbeat rejected: %v", err)
	}
}

// An empty-ID heartbeat names a LAUNCHING shard by instance id and the
// coordinator then dials the address it supplies with the shard secret, so it
// must be refused without the fleet secret. The public port is open to the
// internet.
func TestHeartbeat_EmptyShardID_WithoutFleetSecret_Rejected(t *testing.T) {
	for name, ctx := range map[string]context.Context{
		"no bearer":    heartbeatCtx(""),
		"wrong bearer": heartbeatCtx("guess"),
	} {
		coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}})
		srv := &Server{Coord: coord, ShardSecret: "fleet-secret"}
		_, err := srv.Heartbeat(ctx, &pb.ShardHeartbeatRequest{InstanceId: "i-1", Addr: "6.6.6.6:9000"})
		if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
			t.Errorf("%s: got %v, want Unauthenticated", name, err)
		}
	}
}

// A coordinator with no secret configured must refuse, never wave beats through.
func TestHeartbeat_EmptyShardID_NoSecretConfigured_Rejected(t *testing.T) {
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}})
	srv := &Server{Coord: coord}
	_, err := srv.Heartbeat(heartbeatCtx("anything"), &pb.ShardHeartbeatRequest{InstanceId: "i-1", Addr: "6.6.6.6:9000"})
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("got %v, want Unauthenticated", err)
	}
}

// TestHeartbeat_EmptyToken_Rejected: shards whose registry entry carries an
// empty HeartbeatToken can't be authenticated, so we reject rather than
// waving them through.
func TestHeartbeat_EmptyToken_Rejected(t *testing.T) {
	srv := newRegisteredShard(t, "shard-legacy", "")
	_, err := srv.Heartbeat(context.Background(), &pb.ShardHeartbeatRequest{
		ShardId: "shard-legacy",
		Addr:    "10.0.0.1:9000",
	})
	if err == nil {
		t.Fatal("empty-token shard heartbeat should be rejected")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

// TestHeartbeat_UnknownShardID_Rejected — an unknown shard id (not in
// registry, not in Store) is an unauthenticated request. The legitimate
// "registry empty after coord restart" case falls back to a Store lookup
// (TestHeartbeat_UnknownInRegistry_LoadFromStore).
func TestHeartbeat_UnknownShardID_Rejected(t *testing.T) {
	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{Compute: &compute.Engine{}})
	srv := &Server{Coord: coord}

	_, err := srv.Heartbeat(context.Background(), &pb.ShardHeartbeatRequest{
		ShardId: "shard-unknown",
		Addr:    "10.0.0.2:9000",
	})
	if err == nil {
		t.Fatal("unknown shard heartbeat should be rejected")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

// TestHeartbeat_UnknownInRegistry_LoadFromStore covers the
// rebind-after-coord-restart path: live shards aren't reloaded into
// the in-memory registry on coord boot (LoadFromStore filters to
// ARCHIVED), so the first heartbeat from a live shard arrives with the
// registry empty for that id. The auth path must look the token up in
// the persisted ShardRecord — otherwise tightening the grandfather
// clause would lock the fleet out of the coordinator after every
// rolling restart.
func TestHeartbeat_UnknownInRegistry_LoadFromStore(t *testing.T) {
	env := newSvcEnv(t)

	const shardID = "shard-restart-rebind"
	token, err := shardstore.GenerateShardHeartbeatToken()
	if err != nil {
		t.Fatal(err)
	}
	// The store seeds the default bucket; use it rather than inserting a second.
	bucket, ok, err := env.st.Shards.GetBucketByLabel("default")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		bucket = shardstore.Bucket{Label: "default", TargetIngestingCount: 1}
		if err := env.st.Shards.SaveBucket(&bucket); err != nil {
			t.Fatal(err)
		}
	}
	if err := env.st.Shards.SaveShard(&logdb.ShardRow{
		ID:             shardID,
		WindowStart:    time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		WindowEnd:      time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC),
		State:          logdb.StateIngesting,
		HeartbeatToken: token,
		Tier:           "small",
		BucketID:       bucket.ID,
		CreatedAt:      time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seed shard record: %v", err)
	}

	// Pass Store via CoordinatorConfig so NewCoordinator wires it into
	// the provisioner's ShardOps — the auth fallback path now reads
	// the persisted token through the provisioner, not directly off
	// Server.Store (big lift moved auth into Provisioner.AuthenticateShard).
	srv := env.server(coordinator.CoordinatorConfig{}, nil)

	_, err = srv.Heartbeat(heartbeatCtx(token), &pb.ShardHeartbeatRequest{
		ShardId: shardID,
		Addr:    "10.0.0.5:9000",
		State:   "INGESTING",
	})
	if err != nil {
		t.Fatalf("post-restart heartbeat with valid persisted token rejected: %v", err)
	}

	// And bad bearer is still rejected on the same path.
	_, err = srv.Heartbeat(heartbeatCtx("not-the-real-token"), &pb.ShardHeartbeatRequest{
		ShardId: shardID,
		Addr:    "10.0.0.5:9000",
	})
	if err == nil {
		t.Fatal("bad bearer against persisted token should be rejected")
	}
	if s, _ := status.FromError(err); s.Code() != codes.Unauthenticated {
		t.Errorf("expected Unauthenticated, got %s", s.Code())
	}
}

func TestHeartbeat_FieldFactsAreFoldedAndAcked(t *testing.T) {
	srv := newRegisteredShard(t, "shard-a", "good-token")
	resp, err := srv.Heartbeat(heartbeatCtx("good-token"), &pb.ShardHeartbeatRequest{
		ShardId: "shard-a",
		Addr:    "10.0.0.1:9000",
		State:   "INGESTING",
		FieldFacts: []*pb.FieldFactDelta{
			{CustomerId: 1, Namespace: "ns", Service: "svc", Kind: "main", Env: "prod", Locator: "$5",
				State: pb.FieldFactState_FIELD_FACT_STATE_TYPED, Epoch: 4},
			{CustomerId: 1, Namespace: "ns", Service: "svc", Kind: "main", Env: "prod", Locator: "$6",
				State: pb.FieldFactState_FIELD_FACT_STATE_TYPED, Epoch: 7},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FieldFactsAcked != 7 {
		t.Errorf("acked = %d, want the highest epoch, 7", resp.FieldFactsAcked)
	}
}

func TestHeartbeat_NoFieldFactsAcksNothing(t *testing.T) {
	srv := newRegisteredShard(t, "shard-a", "good-token")
	resp, err := srv.Heartbeat(heartbeatCtx("good-token"), &pb.ShardHeartbeatRequest{ShardId: "shard-a", State: "INGESTING"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.FieldFactsAcked != 0 {
		t.Errorf("acked = %d with nothing to ack", resp.FieldFactsAcked)
	}
}
