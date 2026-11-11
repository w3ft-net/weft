package planner

import (
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	coords "github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shape/shapetest"
)

// The tenant predicate is generated from the build's shape: one equality per
// partition column, each with its value as an argument ahead of the time
// bounds, and none at all in a shape that has no tenant. A scope that names two
// tenants has no single one to plan for.
func TestPlan_TenantPredicateFollowsTheShape(t *testing.T) {
	cols := coords.BuildLayout().Columns
	plan, err := Plan("cat /log/acme/api/main.log", shapetest.Tenant(7), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	var tenantArgs []ArgSource
	for _, a := range plan.ArgSources {
		if a.Kind == ArgPartition {
			tenantArgs = append(tenantArgs, a)
		}
	}
	if len(tenantArgs) != len(cols) {
		t.Fatalf("%d tenant arguments for %d partition columns: %+v", len(tenantArgs), len(cols), plan.ArgSources)
	}
	for i, c := range cols {
		want := c.Name + " = CAST(? AS " + c.Type.SQL() + ")"
		if strings.Count(plan.ShardSQL, want) != 1 {
			t.Errorf("column %s: %q is not in the SQL exactly once:\n%s", c.Name, want, plan.ShardSQL)
		}
		if tenantArgs[i].Part != i || tenantArgs[i].Value != shapetest.Partition(7)[i] {
			t.Errorf("column %s: argument %+v, want part %d value %s", c.Name, tenantArgs[i], i, shapetest.Partition(7)[i])
		}
		if plan.ArgSources[i].Kind != ArgPartition {
			t.Errorf("tenant argument %d is not at the head of the arguments: %+v", i, plan.ArgSources)
		}
	}
	if !slices.Equal(plan.SourcePartition, shapetest.Partition(7)) {
		t.Errorf("SourcePartition = %v, want %v", plan.SourcePartition, shapetest.Partition(7))
	}
	if len(cols) == 0 && strings.Contains(plan.ShardSQL, "CAST(? AS BIGINT)") {
		t.Errorf("a shape with no tenant still filters on one:\n%s", plan.ShardSQL)
	}

	if len(cols) > 0 {
		both := append(shapetest.Tenant(1), shapetest.Tenant(2)...)
		if _, err := Plan("cat /log/acme/api/main.log", both, "", time.Time{}, time.Time{}); !errors.Is(err, coords.ErrMixedPartitions) {
			t.Errorf("a scope of two tenants gave %v, want ErrMixedPartitions", err)
		}
	}
}

// An empty scope reads nothing: it is the deny the principal gets for a source
// that does not exist, before anything is routed.
func TestPlan_EmptyScopeIsADeny(t *testing.T) {
	for _, scope := range []coords.Set{nil, {}} {
		if _, err := Plan("cat /log/acme/api/main.log", scope, "", time.Time{}, time.Time{}); !errors.Is(err, ErrNoSource) {
			t.Errorf("Plan with scope %#v = %v, want ErrNoSource", scope, err)
		}
	}
}

// A plan stored before the tenant came from the shape holds customer_id and an
// ArgCustomer argument. It must still load and hydrate to the SQL arguments the
// planner produces today, or every saved cron falls back to planning inline.
func TestLoweredPlan_ReadsAPlanStoredBeforeThePartition(t *testing.T) {
	b, err := os.ReadFile("testdata/lowered_plan_customer7.json")
	if err != nil {
		t.Fatal(err)
	}
	var lp LoweredPlan
	err = json.Unmarshal(b, &lp)

	cols := coords.BuildLayout().Columns
	if len(cols) != 1 || cols[0].Type != coords.TypeInt64 {
		// A plan stored for a customer cannot be used by a shape with another
		// tenant: it is refused, and the caller plans inline.
		if err == nil {
			t.Fatalf("a plan stored for a customer loaded under a shape with %d partition columns", len(cols))
		}
		return
	}
	if err != nil {
		t.Fatalf("an old stored plan did not load: %v", err)
	}
	if !slices.Equal(lp.Partition, []string{"7"}) {
		t.Fatalf("Partition = %v, want [7]", lp.Partition)
	}

	start := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	end := time.Date(2026, 1, 2, 5, 4, 5, 0, time.UTC)
	args := lp.Hydrate(ResolveCtx{Partition: lp.Partition, Start: start, End: end})
	fresh, err := Plan(`cat /log/acme/api/main.log | grep ERROR | awk '$5 > 500'`, shapetest.Tenant(7), "", start, end)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != len(fresh.Args) {
		t.Fatalf("hydrated %d arguments, a fresh plan has %d:\n%v\n%v", len(args), len(fresh.Args), args, fresh.Args)
	}
	for i := range args {
		if args[i] != fresh.Args[i] {
			t.Errorf("argument %d hydrates to %v, a fresh plan has %v", i, args[i], fresh.Args[i])
		}
	}
	if lp.ShardSQL != fresh.ShardSQL {
		t.Errorf("the stored SQL differs from today's:\n stored: %s\n fresh:  %s", lp.ShardSQL, fresh.ShardSQL)
	}
}

// A stored plan this build cannot use is an error, not a plan that runs with a
// missing tenant: the caller then plans the pipeline inline.
func TestLoweredPlan_RefusesAPlanThatDoesNotFitTheShape(t *testing.T) {
	cols := len(coords.BuildLayout().Columns)
	tooMany := make([]string, cols+1)
	body, _ := json.Marshal(map[string]any{"partition": tooMany, "shard_sql": "x"})
	var lp LoweredPlan
	if err := json.Unmarshal(body, &lp); err == nil {
		t.Error("a plan with the wrong number of partition values loaded")
	}

	if cols > 0 {
		body, _ := json.Marshal(map[string]any{
			"partition": shapetest.Partition(1),
			"args":      []map[string]any{{"Kind": int(ArgPartition), "Value": "1", "Part": cols}},
		})
		if err := json.Unmarshal(body, &lp); err == nil {
			t.Error("a plan with an argument for a partition value it lacks loaded")
		}
	}
}
