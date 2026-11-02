package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// swap-archive is the last step of converting an archive: point the shard at the
// converted copy. It reads before it writes and does nothing unless told to.

type fakeAdmin struct {
	pb.WeftAdminClient
	path  string // the shard's parquet path
	state string
	swaps []*pb.SwapShardArchiveRequest
}

func (f *fakeAdmin) GetShard(_ context.Context, in *pb.GetShardRequest, _ ...grpc.CallOption) (*pb.GetShardResponse, error) {
	return &pb.GetShardResponse{Shard: &pb.ShardInfo{Id: in.ShardId, State: f.state, ParquetPath: f.path}}, nil
}

func (f *fakeAdmin) SwapShardArchive(_ context.Context, in *pb.SwapShardArchiveRequest, _ ...grpc.CallOption) (*pb.SwapShardArchiveResponse, error) {
	f.swaps = append(f.swaps, in)
	return &pb.SwapShardArchiveResponse{}, nil
}

const (
	swapOldPath = "s3://b/40-apps/shard-1.attempt-1/m1440/**/*.parquet"
	swapNewPath = "s3://b/40-apps/shard-1.attempt-1.r9/sized/**/*.parquet"
)

func TestSwapArchive_DryRunChangesNothing(t *testing.T) {
	f := &fakeAdmin{path: swapOldPath, state: "ARCHIVED"}
	var out bytes.Buffer
	if err := swapArchive(context.Background(), f, "shard-1", swapOldPath, swapNewPath, false, &out); err != nil {
		t.Fatal(err)
	}
	if len(f.swaps) != 0 {
		t.Errorf("a dry run swapped: %+v", f.swaps)
	}
	if !strings.Contains(out.String(), "dry run") {
		t.Errorf("output does not say it was a dry run:\n%s", out.String())
	}
}

func TestSwapArchive_SwapsWhenTheShardIsAtTheOldPath(t *testing.T) {
	f := &fakeAdmin{path: swapOldPath, state: "ARCHIVED"}
	if err := swapArchive(context.Background(), f, "shard-1", swapOldPath, swapNewPath, true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(f.swaps) != 1 || f.swaps[0].ShardId != "shard-1" || f.swaps[0].OldPath != swapOldPath || f.swaps[0].NewPath != swapNewPath {
		t.Errorf("swaps = %+v", f.swaps)
	}
}

// If the shard is somewhere else the command stops before the RPC, so a wrong
// shard id or a stale plan never gets as far as the coordinator.
func TestSwapArchive_RefusesAShardAtAnotherPath(t *testing.T) {
	f := &fakeAdmin{path: "s3://b/something-else.parquet", state: "ARCHIVED"}
	err := swapArchive(context.Background(), f, "shard-1", swapOldPath, swapNewPath, true, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "something-else") {
		t.Fatalf("err = %v, want a refusal naming where the shard is", err)
	}
	if len(f.swaps) != 0 {
		t.Errorf("swapped anyway: %+v", f.swaps)
	}
}

// A coordinator whose registry is already at the new path has nothing to do.
func TestSwapArchive_AlreadySwappedIsNotAnError(t *testing.T) {
	f := &fakeAdmin{path: swapNewPath, state: "ARCHIVED"}
	var out bytes.Buffer
	if err := swapArchive(context.Background(), f, "shard-1", swapOldPath, swapNewPath, true, &out); err != nil {
		t.Fatal(err)
	}
	if len(f.swaps) != 0 || !strings.Contains(out.String(), "already") {
		t.Errorf("swaps %+v, output %q: want nothing sent and a note that it is already swapped", f.swaps, out.String())
	}
}
