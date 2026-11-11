package logdb

import (
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logtable"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// testLegacy is how a deployment with customer-keyed shards names a source's
// table: its one partition column is the customer.
func testLegacy(customerID int64, namespace, service, kind string) string {
	return logtable.Name([]any{customerID}, namespace, service, kind)
}

var prodKey = shard.FactKey{Table: testLegacy(1, "ns", "svc", "main"), Env: "prod", Locator: "$5"}

// delta is what a shard of this build reports for k, a source of customer 1.
func delta(k shard.FactKey, state pb.FieldFactState, epoch uint64) *pb.FieldFactDelta {
	d := legacyDelta(k, state, epoch)
	d.Table = k.Table
	return d
}

// legacyDelta is what a shard that predates table names reports: the source
// by customer_id, namespace, service and kind.
func legacyDelta(k shard.FactKey, state pb.FieldFactState, epoch uint64) *pb.FieldFactDelta {
	return &pb.FieldFactDelta{CustomerId: 1, Namespace: "ns", Service: "svc", Kind: "main",
		Env: k.Env, Locator: k.Locator, State: state, Epoch: epoch}
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
	reg.SetLegacyFactTable(testLegacy)
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
	boot.SetLegacyFactTable(testLegacy)
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

// A shard that predates table names says which source a fact is about by
// customer_id, namespace, service and kind. The coordinator's hook names the
// same table the new shard reports, so a fleet in the middle of an upgrade
// agrees on the facts.
func TestFoldFieldFacts_OldShardsFactsLandInTheTableANewShardWouldReport(t *testing.T) {
	reg, _ := registryWithShard(t)
	if acked := reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{legacyDelta(prodKey, typed, 3)}); acked != 3 {
		t.Errorf("acked = %d, want 3", acked)
	}
	if !reg.Get("s1").FieldTyped(prodKey) {
		t.Fatal("a fact from a shard with no table name is not held under the table the planner asks for")
	}
	// and the new shard's retraction of it, by table, reaches the same entry
	reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, dirty, 9)})
	if reg.Get("s1").FieldTyped(prodKey) {
		t.Error("a retraction by table did not reach a fact held from a legacy delta")
	}
}

// A build that has no customer-keyed shards supplies no hook: such facts are
// unknown, which plans as it always has, and are never stored.
func TestFoldFieldFacts_WithoutAHookAFactWithNoTableIsUnknown(t *testing.T) {
	reg, fake := registryWithShard(t)
	reg.SetLegacyFactTable(nil)
	reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{legacyDelta(prodKey, typed, 3)})
	if reg.Get("s1").FieldTyped(prodKey) {
		t.Error("a fact with no table became typed without a hook to name it")
	}
	if rows, _ := fake.ListShardFieldFacts(); len(rows) != 0 {
		t.Errorf("an unresolvable fact was stored: %+v", rows)
	}
	// a shard of this build needs no hook
	reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, typed, 4)})
	if !reg.Get("s1").FieldTyped(prodKey) {
		t.Error("a fact with a table was not held")
	}
}

func TestShardFieldsTyped_AsksByTable(t *testing.T) {
	reg, _ := registryWithShard(t)
	p := NewProvisioner(reg, nil, ProvisionerConfig{})
	reg.FoldFieldFacts("s1", []*pb.FieldFactDelta{delta(prodKey, typed, 3)})
	if !p.ShardFieldsTyped("s1", prodKey.Table, "prod", []string{"$5"}) {
		t.Error("typed fact not found by table")
	}
	if p.ShardFieldsTyped("s1", testLegacy(2, "ns", "svc", "main"), "prod", []string{"$5"}) {
		t.Error("another customer's table reads as typed")
	}
}
