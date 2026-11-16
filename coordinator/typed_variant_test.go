package coordinator

import (
	"testing"
	"time"

	"github.com/w3ft-net/weft/shape/shapetest"

	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/logdb"
	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// typedFact is a typed fact as a shard of this build reports it: under the
// source's logical table, with the readable parts beside it.
func typedFact(customer int64, ns, svc, kind, env, locator string, epoch uint64) *pb.FieldFactDelta {
	return &pb.FieldFactDelta{CustomerId: customer, Namespace: ns, Service: svc, Kind: kind, Env: env, Locator: locator,
		Table: shapetest.Table(customer, ns, svc, kind),
		State: pb.FieldFactState_FIELD_FACT_STATE_TYPED, Epoch: epoch}
}

func TestApplyTypedVariant_OnlyShardsWhoseFactsAllowIt(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}})
	for _, id := range []string{"typed", "other-env", "other-field", "no-facts"} {
		logdb.SeedShard(c.Provisioner, &logdb.Shard{
			ShardRow: logdb.ShardRow{
				ID:        id,
				State:     logdb.StateIngesting,
				CreatedAt: time.Now(),
			},
		})
	}
	fold := func(id string, f ...*pb.FieldFactDelta) { c.Provisioner.FoldFieldFacts(id, f) }
	fold("typed", typedFact(7, "acme", "api", "main", "", "$5", 1))
	fold("other-env", typedFact(7, "acme", "api", "main", "staging", "$5", 1))
	fold("other-field", typedFact(7, "acme", "api", "main", "", "$4", 1))

	plan, err := planner.Plan(`cat /log/acme/api/main.log | awk '$5 > 500'`, shapetest.Group(7), "", time.Time{}, time.Time{})
	if err != nil || plan.Typed == nil {
		t.Fatalf("plan: %v, typed %+v", err, plan.Typed)
	}
	descs := []*pb.ShardDescriptor{{ShardId: "typed"}, {ShardId: "other-env"}, {ShardId: "other-field"}, {ShardId: "no-facts"}, {ShardId: "not-in-registry"}}
	args := []string{"a", "b"}

	if n := c.applyTypedVariant(plan, args, descs); n != 1 {
		t.Fatalf("%d shards got the variant, want 1", n)
	}
	for _, d := range descs {
		got := d.Sql != ""
		if want := d.ShardId == "typed"; got != want {
			t.Errorf("shard %s: variant=%v, want %v", d.ShardId, got, want)
		}
	}
	if descs[0].Sql != plan.Typed.DirectShardSQL || len(descs[0].Args) != 2 {
		t.Errorf("typed shard got sql=%q args=%v", descs[0].Sql, descs[0].Args)
	}
}

// Every locator the variant needs must be typed, not just one of them.
func TestApplyTypedVariant_NeedsEveryLocator(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}})
	for _, id := range []string{"both", "one"} {
		logdb.SeedShard(c.Provisioner, &logdb.Shard{
			ShardRow: logdb.ShardRow{
				ID:        id,
				State:     logdb.StateIngesting,
				CreatedAt: time.Now(),
			},
		})
	}
	c.Provisioner.FoldFieldFacts("both", []*pb.FieldFactDelta{
		typedFact(7, "acme", "api", "main", "", "$3", 1), typedFact(7, "acme", "api", "main", "", "$5", 2)})
	c.Provisioner.FoldFieldFacts("one", []*pb.FieldFactDelta{typedFact(7, "acme", "api", "main", "", "$3", 1)})

	plan, err := planner.Plan(`cat /log/acme/api/main.log | awk '$3 > 1 && $5 < 100'`, shapetest.Group(7), "", time.Time{}, time.Time{})
	if err != nil || plan.Typed == nil {
		t.Fatalf("plan: %v", err)
	}
	descs := []*pb.ShardDescriptor{{ShardId: "both"}, {ShardId: "one"}}
	if n := c.applyTypedVariant(plan, nil, descs); n != 1 || descs[0].Sql == "" || descs[1].Sql != "" {
		t.Errorf("n=%d, both=%q one=%q", n, descs[0].Sql, descs[1].Sql)
	}
}

func TestApplyTypedVariant_NoVariantNoChange(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}})
	plan, err := planner.Plan(`cat /log/acme/api/main.log | grep error`, shapetest.Group(7), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	descs := []*pb.ShardDescriptor{{ShardId: "x"}}
	if n := c.applyTypedVariant(plan, nil, descs); n != 0 || descs[0].Sql != "" {
		t.Errorf("n=%d sql=%q for a plan with no variant", n, descs[0].Sql)
	}
}

// A query whose every shard has the facts runs the typed plan's whole
// exactly-planned form; one shard without them keeps the base plan.
func TestExactPlan_OnlyWhenEveryShardHasTheFacts(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}})
	for _, id := range []string{"a", "b", "old"} {
		logdb.SeedShard(c.Provisioner, &logdb.Shard{
			ShardRow: logdb.ShardRow{
				ID:        id,
				State:     logdb.StateIngesting,
				CreatedAt: time.Now(),
			},
		})
	}
	for _, id := range []string{"a", "b"} {
		c.Provisioner.FoldFieldFacts(id, []*pb.FieldFactDelta{typedFact(7, "acme", "api", "main", "", "$3", 1)})
	}
	plan, err := planner.Plan(`cat /log/acme/api/main.log | awk '$3 > 100 {print $1, $3}'`, shapetest.Group(7), "", time.Time{}, time.Time{})
	if err != nil || plan.Typed == nil || plan.Typed.Exact == nil {
		t.Fatalf("plan: %v, typed %+v", err, plan.Typed)
	}
	if len(plan.ComputeStages) == 0 || len(plan.Typed.Exact.ComputeStages) != 0 {
		t.Fatalf("base stages %v, exact stages %v: the test needs the exact plan to drop the stage",
			plan.ComputeStages, plan.Typed.Exact.ComputeStages)
	}

	if got := c.exactPlan(plan, []*pb.ShardDescriptor{{ShardId: "a"}, {ShardId: "b"}}); got != plan.Typed.Exact {
		t.Errorf("every shard typed: got %v, want the exact plan", got)
	}
	if got := c.exactPlan(plan, []*pb.ShardDescriptor{{ShardId: "a"}, {ShardId: "old"}}); got != nil {
		t.Errorf("one shard without the facts still got the exact plan")
	}
	if got := c.exactPlan(plan, nil); got != nil {
		t.Errorf("no shards: got an exact plan")
	}
	noVariant, err := planner.Plan(`cat /log/acme/api/main.log | grep error`, shapetest.Group(7), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.exactPlan(noVariant, []*pb.ShardDescriptor{{ShardId: "a"}}); got != nil {
		t.Errorf("a plan with no typed form got an exact plan")
	}
}

// A typed shard's override carries the field form too, with the same typed
// predicate, or it would run the typed filter in one form and not the other.
func TestApplyTypedVariant_CarriesTheFieldForm(t *testing.T) {
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}})
	logdb.SeedShard(c.Provisioner, &logdb.Shard{
		ShardRow: logdb.ShardRow{
			ID:        "typed",
			State:     logdb.StateIngesting,
			CreatedAt: time.Now(),
		},
	})
	c.Provisioner.FoldFieldFacts("typed", []*pb.FieldFactDelta{typedFact(7, "acme", "api", "main", "", "$5", 1)})

	plan, err := planner.Plan(`cat /log/acme/api/main.log | awk '$5 > 500 { n += $2 } END { print n }'`, shapetest.Group(7), "", time.Time{}, time.Time{})
	if err != nil || plan.Typed == nil || plan.FieldSQL == "" {
		t.Fatalf("plan: %v, typed %+v, field sql %q", err, plan.Typed, plan.FieldSQL)
	}
	if plan.Typed.FieldSQL == "" || plan.Typed.FieldSQL == plan.FieldSQL {
		t.Fatalf("the typed plan's field form %q is not the typed form of %q", plan.Typed.FieldSQL, plan.FieldSQL)
	}
	descs := []*pb.ShardDescriptor{{ShardId: "typed"}}
	if n := c.applyTypedVariant(plan, nil, descs); n != 1 || descs[0].FieldSql != plan.Typed.FieldSQL {
		t.Errorf("n=%d field sql=%q, want the typed plan's %q", n, descs[0].FieldSql, plan.Typed.FieldSQL)
	}
}
