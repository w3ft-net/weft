package main

import (
	"reflect"
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
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
		{Id: "shard-archived", State: "ARCHIVED"}, // skip: nothing to upgrade
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
