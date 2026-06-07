package main

import (
	"reflect"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// liveShardIDs is what roll-cohort uses to decide which shards to roll and
// in what order. Filtering is by state (only INGESTING / READ_ONLY /
// DEGRADED — see Provisioner.RelaunchShard); order is sorted-by-id so the
// roll is deterministic regardless of how ListShards returned them.
func TestLiveShardIDs_FiltersAndSorts(t *testing.T) {
	in := []*pb.ShardInfo{
		{Id: "shard-3", State: "INGESTING"},
		{Id: "shard-1", State: "READ_ONLY"},
		{Id: "shard-launching", State: "LAUNCHING"}, // skip: already on current AMI
		{Id: "shard-2", State: "DEGRADED"},
		{Id: "shard-archived", State: "ARCHIVED"},   // skip: nothing to upgrade
		{Id: "shard-suspended", State: "SUSPENDED"}, // skip: nothing to upgrade
		{Id: "shard-archiving", State: "ARCHIVING"}, // skip
		{Id: "shard-gone", State: "GONE"},           // skip
	}
	want := []string{"shard-1", "shard-2", "shard-3"}
	got := liveShardIDs(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("liveShardIDs = %v, want %v", got, want)
	}
}

func TestLiveShardIDs_EmptyWhenNoneLive(t *testing.T) {
	in := []*pb.ShardInfo{
		{Id: "a", State: "LAUNCHING"},
		{Id: "b", State: "ARCHIVED"},
	}
	got := liveShardIDs(in)
	if len(got) != 0 {
		t.Fatalf("expected no live shards, got %v", got)
	}
}

// shardsToRoll layers the --target-sha skip on top of liveShardIDs:
// when target != "", any shard whose binary_sha already matches is skipped
// (the deploy is a no-op for it). target == "" rolls every live shard.
func TestShardsToRoll_SkipsMatchingSHA(t *testing.T) {
	in := []*pb.ShardInfo{
		{Id: "a", State: "INGESTING", BinarySha: "newsha"}, // skip
		{Id: "b", State: "INGESTING", BinarySha: "oldsha"}, // roll
		{Id: "c", State: "INGESTING", BinarySha: ""},       // roll (unknown → can't trust)
		{Id: "d", State: "READ_ONLY", BinarySha: "newsha"}, // skip
		{Id: "e", State: "DEGRADED", BinarySha: "oldsha"},  // roll
		{Id: "f", State: "LAUNCHING", BinarySha: "oldsha"}, // skip: not live-with-data
	}
	got := shardsToRoll(in, "newsha")
	want := []string{"b", "c", "e"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shardsToRoll = %v, want %v", got, want)
	}
}

func TestShardsToRoll_EmptyTargetRollsEveryLiveShard(t *testing.T) {
	in := []*pb.ShardInfo{
		{Id: "a", State: "INGESTING", BinarySha: "x"},
		{Id: "b", State: "READ_ONLY", BinarySha: "x"},
	}
	got := shardsToRoll(in, "")
	want := []string{"a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shardsToRoll(target=\"\") = %v, want %v", got, want)
	}
}
