// Package heartbeat is the shard's side of the heartbeat to the coordinator: the
// request a shard builds each tick (its identity, its row count and disk
// footprint, the field facts it has not had acknowledged) and the call that
// sends it with the right bearer token.
package heartbeat

import (
	"context"
	"time"

	"google.golang.org/grpc/metadata"

	weftsdk "github.com/w3ft-net/weft-go"
	"github.com/w3ft-net/weft/backends/logs/live"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
	shardsvc "github.com/w3ft-net/weft/shard/service"
)

// Build materializes the proto from identity snapshot.
// Pulled out so unit tests can assert every outbound field without a
// live gRPC client. The coordinator's christen path needs InstanceId
// to find the pre-attached EBS volume (tag:shard_id=pending-<id>) —
// an empty InstanceId silently drops that lookup.
func Build(shardID, addr, instanceID, state, binarySHA string, rowCount, diskBytes int64) *pb.ShardHeartbeatRequest {
	return &pb.ShardHeartbeatRequest{
		ShardId:    shardID,
		Addr:       addr,
		InstanceId: instanceID,
		State:      state,
		RowCount:   rowCount,
		DiskBytes:  diskBytes, // .duckdb + spill footprint; control-plane view of volume pressure
		BinarySha:  binarySHA, // the build's commit, injected at link time; empty on local builds
	}
}

// For builds this tick's heartbeat, or reports there is none to
// send: a paused shard has handed its volume to a successor, and a beat
// from its old address would pull callers back to it.
func For(srv *shardsvc.Server, addr, instanceID, binarySHA string) (*pb.ShardHeartbeatRequest, bool) {
	if srv.Paused() {
		return nil, false
	}
	shardID := srv.Identity()
	state := "INGESTING"
	if shardID == "" {
		state = "ADOPTED"
	}
	var rowCount, diskBytes int64
	if ds := srv.Shard(); ds != nil {
		rowCount = ds.RowCount()
		diskBytes = ds.DiskUsage().TotalBytes()
	}
	req := Build(shardID, addr, instanceID, state, binarySHA, rowCount, diskBytes)
	if ds := srv.Shard(); ds != nil && !srv.Spare() {
		if f, ok := ds.(live.FieldFactSource); ok {
			req.FieldFacts = FieldFactDeltas(f.FieldFacts())
		}
		req.LayoutFingerprint = ds.Layout().Fingerprint
	}
	if srv.Spare() {
		req.State = "SPARE"
		req.Spare = true
	}
	return req, true
}

// Send sends one heartbeat and reports whether it went (or there was none to
// send). A shard not yet christened presents shardSecret, the fleet secret every
// shard boots with, in place of its own token.
func Send(client pb.WeftCoordinatorClient, addr, instanceID, binarySHA string, srv *shardsvc.Server, shardSecret string) bool {
	req, send := For(srv, addr, instanceID, binarySHA)
	if !send {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Attach the per-shard bearer token so the coordinator's Heartbeat
	// handler can prove this heartbeat came from the real owner of
	// shardID. A shard not yet christened has no token of its own, so it
	// presents the fleet secret every shard boots with instead.
	tok := srv.HeartbeatToken()
	if tok == "" {
		tok = shardSecret
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)

	resp, err := client.Heartbeat(ctx, req)
	if err != nil {
		weftsdk.Infof(ctx, "heartbeat: %v", err)
		return false
	}
	if ds := srv.Shard(); ds != nil {
		if f, ok := ds.(live.FieldFactSource); ok {
			f.AckFieldFacts(resp.FieldFactsAcked)
		}
	}
	return true
}

// FieldFactDeltas puts the shard's unacknowledged field facts on the wire.
func FieldFactDeltas(facts []shard.FieldFact) []*pb.FieldFactDelta {
	out := make([]*pb.FieldFactDelta, len(facts))
	for i, f := range facts {
		state := pb.FieldFactState_FIELD_FACT_STATE_TYPED
		if f.State == shard.FactDirty {
			state = pb.FieldFactState_FIELD_FACT_STATE_DIRTY
		}
		out[i] = &pb.FieldFactDelta{
			CustomerId: f.Customer,
			Table:      f.Table,
			Namespace:  f.Namespace,
			Service:    f.Service,
			Kind:       f.Kind,
			Env:        f.Env,
			Locator:    f.Locator,
			State:      state,
			Epoch:      f.Epoch,
		}
	}
	return out
}
