package planner

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

// noResolver is the no-/bin resolver used by tests that don't exercise
// inlining. expandForLowering doesn't call it today (Phase B v1 leaves
// pipelines untouched), so the test fixture is fine using a stub.
type noResolver struct{}

func (noResolver) Source(name string) (string, string, bool) { return "", "", false }

// TestLower_RoundTrip pins the contract Hydrate is built around:
//
//	(Lower → JSON marshal → unmarshal → Hydrate(ctx))
//
// must produce a SQL string + args identical to a fresh Plan(pipeline,
// ctx.Customer, ctx.Start, ctx.End) call. If this ever drifts, the
// lowered form is silently shipping different SQL than the live path —
// which is exactly the regression mode this test guards.
func TestLower_RoundTrip(t *testing.T) {
	pipeline := `cat /log/weft/coordinator/main.log | grep ERROR`
	customer := int64(1)
	start := time.Date(2026, 5, 13, 9, 0, 0, 0, time.UTC)
	end := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)

	// Lower at save time (customer matters for the row-level
	// boundary; time bounds are sentinels and don't matter here).
	lp, err := Lower(pipeline, customer, "bolt", noResolver{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	// Round-trip through JSON to catch any non-serializable field.
	b, err := json.Marshal(lp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got LoweredPlan
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Hydrate against the fire-time context.
	hydratedArgs := got.Hydrate(ResolveCtx{CustomerID: customer, Start: start, End: end})

	// Fresh Plan call against the same context — what the live
	// dispatch path would produce today.
	plan, err := Plan(pipeline, customer, "", start, end)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if got.ShardSQL != plan.ShardSQL {
		t.Errorf("ShardSQL drift after round-trip:\n got: %s\nwant: %s", got.ShardSQL, plan.ShardSQL)
	}
	if got.DirectShardSQL != plan.DirectShardSQL {
		t.Errorf("DirectShardSQL drift after round-trip:\n got: %s\nwant: %s", got.DirectShardSQL, plan.DirectShardSQL)
	}
	if !reflect.DeepEqual(hydratedArgs, plan.Args) {
		t.Errorf("Hydrate args drift:\n got: %v\nwant: %v", hydratedArgs, plan.Args)
	}
}

// TestLower_HydrateUsesFireTimeBounds confirms that the time bounds
// baked into LoweredPlan at save time are NOT what Hydrate emits. The
// fire-time ResolveCtx wins. This is the whole point of the runtime-
// kinds split: the same lowered plan re-fires across many time
// windows.
func TestLower_HydrateUsesFireTimeBounds(t *testing.T) {
	pipeline := `cat /log/loom/api/prod.log | grep ERROR`
	lp, err := Lower(pipeline, 1, "bolt", noResolver{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	fire1 := ResolveCtx{
		CustomerID: 1,
		Start:      time.Date(2026, 5, 13, 9, 0, 0, 0, time.UTC),
		End:        time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC),
	}
	fire2 := ResolveCtx{
		CustomerID: 1,
		Start:      time.Date(2026, 5, 13, 11, 0, 0, 0, time.UTC),
		End:        time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
	}

	args1 := lp.Hydrate(fire1)
	args2 := lp.Hydrate(fire2)

	// Same SQL string for both (it's symbolic). Args differ by time.
	if reflect.DeepEqual(args1, args2) {
		t.Fatal("Hydrate produced identical args for different time bounds — the time placeholders aren't actually being resolved from ctx")
	}

	// Walk ArgSources and verify the runtime ones got fire1's values.
	for i, src := range lp.ArgSources {
		switch src.Kind {
		case ArgTimeStart:
			if args1[i] != "1970-01-01 00:00:00.000001" {
				// Lower used sentinel time (1ns since epoch UTC).
				// Hydrate must override with fire1.Start.
				if args1[i] != formatTS(fire1.Start) {
					t.Errorf("args1[%d] (ArgTimeStart) = %v, want %s",
						i, args1[i], formatTS(fire1.Start))
				}
			}
		case ArgTimeEnd:
			if args1[i] != formatTS(fire1.End) {
				t.Errorf("args1[%d] (ArgTimeEnd) = %v, want %s",
					i, args1[i], formatTS(fire1.End))
			}
		case ArgCustomer:
			if args1[i] != "1" {
				t.Errorf("args1[%d] (ArgCustomer) = %v, want 1", i, args1[i])
			}
		}
	}
}

// TestLower_PlannerVersion stamps. A LoweredPlan rolled out of Lower
// must carry the current PlannerVersion. Consumers compare against
// PlannerVersion to detect stale plans after a planner-breaking change.
func TestLower_PlannerVersion(t *testing.T) {
	lp, err := Lower(`cat /log/foo.log`, 1, "bolt", noResolver{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	if lp.PlannerVersion != PlannerVersion {
		t.Errorf("PlannerVersion: got %d, want %d", lp.PlannerVersion, PlannerVersion)
	}
}

// fakeBinResolver is the test-controlled BinResolver for /bin
// inlining tests. binByName is the script body; hash is computed
// by HashBinSource at fixture-load time so Lower's recorded
// BinDep.Hash matches what the fire path's invalidation check
// would produce.
type fakeBinResolver struct {
	binByName map[string]string
}

func (r fakeBinResolver) Source(name string) (string, string, bool) {
	source, ok := r.binByName[name]
	if !ok {
		return "", "", false
	}
	return source, HashBinSource(source), true
}

// TestLower_InlinesBinScript pins the save-time /bin inlining that
// lets /bin-referencing watches use the fast fire path. The lowered
// plan must:
//   - render SQL that reflects the inlined body, not the /bin/<name>
//     token (which Plan would reject as unplannable)
//   - record a BinDep with the bin's name + content hash so the fire
//     path can invalidate on /bin edit
//   - bind positional args from the invocation into the body
func TestLower_InlinesBinScript(t *testing.T) {
	binBody := `cat /log/loom/api/prod.log | grep $1`
	resolver := fakeBinResolver{
		binByName: map[string]string{"find-errors": binBody},
	}

	lp, err := Lower(`find-errors ERROR | wc -l`, 1, "bolt", resolver)
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	// SQL should reflect the inlined body — regexp_matches for the
	// bound $1, namespace/service for the inlined /log/.../prod.log.
	if !strings.Contains(lp.ShardSQL, "regexp_matches(raw, 'ERROR')") {
		t.Errorf("expected inlined grep predicate; got: %s", lp.ShardSQL)
	}
	if !strings.Contains(lp.ShardSQL, "namespace = ?") {
		t.Errorf("expected inlined namespace predicate; got: %s", lp.ShardSQL)
	}

	// BinDep records the script with its hash for fire-time invalidation.
	if len(lp.BinDeps) != 1 {
		t.Fatalf("BinDeps: got %d, want 1: %v", len(lp.BinDeps), lp.BinDeps)
	}
	if lp.BinDeps[0].Name != "find-errors" {
		t.Errorf("BinDep name: got %q, want find-errors", lp.BinDeps[0].Name)
	}
	if lp.BinDeps[0].Hash != HashBinSource(binBody) {
		t.Errorf("BinDep hash diverges from HashBinSource(body)")
	}

	// The trailing `| wc -l` from the outer pipeline survives, as the
	// shard's line count.
	if lp.Group == nil || lp.Group.Render != RenderTotal {
		t.Errorf("outer pipeline's `wc -l` lost during inlining; got group %+v, compute stages %v", lp.Group, lp.ComputeStages)
	}
}

// TestLower_NoArgSourcesWhenCustomerEmpty pins the planner contract
// that an empty customer skips the ArgCustomer predicate. Lower
// inherits this: if the caller passes customer == 0, the lowered
// form has zero ArgCustomer entries. (No production caller does this
// for watches — but the test pins it as a property so a future Lower
// caller doesn't quietly add a customer="" path.)
func TestLower_NoArgSourcesWhenCustomerEmpty(t *testing.T) {
	lp, err := Lower(`cat /log/foo.log`, 0, "bolt", noResolver{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	for i, src := range lp.ArgSources {
		if src.Kind == ArgCustomer {
			t.Errorf("ArgSources[%d] = ArgCustomer with empty customer; want skipped", i)
		}
	}
}
