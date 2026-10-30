package logdb

import (
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

var prodKey = shard.FactKey{CustomerID: 1, Namespace: "ns", Service: "svc", Kind: "main", Env: "prod", Locator: "$5"}

func delta(k shard.FactKey, state pb.FieldFactState, epoch uint64) *pb.FieldFactDelta {
	return &pb.FieldFactDelta{CustomerId: k.CustomerID, Namespace: k.Namespace, Service: k.Service,
		Kind: k.Kind, Env: k.Env, Locator: k.Locator, State: state, Epoch: epoch}
}

const (
	typed = pb.FieldFactState_FIELD_FACT_STATE_TYPED
	dirty = pb.FieldFactState_FIELD_FACT_STATE_DIRTY
)

func registryWithShard(t *testing.T) (*ShardRegistry, *fakeRegistryStore) {
	t.Helper()
	fake := newFakeRegistryStore()
	reg := NewShardRegistry()
	reg.SetStore(fake)
	reg.Add(&Shard{ID: "s1", State: StateIngesting, CreatedAt: time.Now()})
	return reg, fake
}

func TestFoldFieldFacts_TypedThenRetracted(t *testing.T) {
	reg, fake := registryWithShard(t)

	if acked := reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, typed, 3)}); acked != 3 {
		t.Errorf("acked = %d, want 3", acked)
	}
	sh := reg.Get("s1")
	if !sh.FieldTyped(prodKey) {
		t.Error("typed fact not held")
	}
	other := prodKey
	other.Env = "staging"
	if sh.FieldTyped(other) {
		t.Error("another env's source reads as typed")
	}
	if rows, _ := fake.ListShardFieldFacts(); len(rows) != 1 || rows[0].Epoch != 3 || rows[0].ShardID != "s1" {
		t.Errorf("store rows = %+v", rows)
	}

	if acked := reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, dirty, 9)}); acked != 9 {
		t.Errorf("acked = %d, want 9", acked)
	}
	if sh.FieldTyped(prodKey) {
		t.Error("retracted fact still typed")
	}
	if rows, _ := fake.ListShardFieldFacts(); len(rows) != 0 {
		t.Errorf("retraction left store rows %+v", rows)
	}
}

func TestFoldFieldFacts_StaleDuplicateCannotResurrectARetractedFact(t *testing.T) {
	reg, _ := registryWithShard(t)
	reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, typed, 3)})
	reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, dirty, 9)})
	reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, typed, 3)})
	if reg.Get("s1").FieldTyped(prodKey) {
		t.Error("an old typed beat re-typed a retracted fact")
	}
}

func TestFoldFieldFacts_UnknownShardIsNotAcked(t *testing.T) {
	reg, _ := registryWithShard(t)
	if acked := reg.FoldFieldFacts("nope", []*pb.FieldFactDelta{delta(prodKey, typed, 3)}); acked != 0 {
		t.Errorf("acked = %d for a shard the registry doesn't hold", acked)
	}
}

func TestFieldFacts_LoadedAtBootAndFollowedOnRefresh(t *testing.T) {
	reg, fake := registryWithShard(t)
	reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, typed, 3)})

	// A second coordinator boots from the same store.
	boot := NewShardRegistry()
	boot.SetStore(fake)
	if err := boot.LoadFromStore(func(*Shard) (shard.Shard, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if sh := boot.Get("s1"); sh == nil || !sh.FieldTyped(prodKey) {
		t.Fatal("a coordinator booted after the beat doesn't know the fact")
	}

	// The first coordinator retracts it; the second follows on refresh.
	reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, dirty, 9)})
	boot.RefreshFromStore(false)
	if boot.Get("s1").FieldTyped(prodKey) {
		t.Error("a follower still holds a retracted fact after refresh")
	}
}
