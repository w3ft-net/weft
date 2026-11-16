package planner

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	coords "github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
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
	lp, err := Lower(pipeline, shapetest.Group(customer), noResolver{})
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
	hydratedArgs := got.Hydrate(ResolveCtx{Partition: shapetest.Partition(customer), Start: start, End: end})

	// Fresh Plan call against the same context — what the live
	// dispatch path would produce today.
	plan, err := Plan(pipeline, shapetest.Group(customer), "", start, end)
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
	lp, err := Lower(pipeline, shapetest.Group(1), noResolver{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}

	fire1 := ResolveCtx{
		Partition: shapetest.Partition(1),
		Start:     time.Date(2026, 5, 13, 9, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC),
	}
	fire2 := ResolveCtx{
		Partition: shapetest.Partition(1),
		Start:     time.Date(2026, 5, 13, 11, 0, 0, 0, time.UTC),
		End:       time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC),
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
		case ArgPartition:
			if want := shapetest.Partition(1)[src.Part]; args1[i] != want {
				t.Errorf("args1[%d] (ArgPartition %d) = %v, want %s", i, src.Part, args1[i], want)
			}
		}
	}
}

// TestLower_PlannerVersion stamps. A LoweredPlan rolled out of Lower
// must carry the current PlannerVersion. Consumers compare against
// PlannerVersion to detect stale plans after a planner-breaking change.
func TestLower_PlannerVersion(t *testing.T) {
	lp, err := Lower(`cat /log/foo.log`, shapetest.Group(1), noResolver{})
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

	lp, err := Lower(`find-errors ERROR | wc -l`, shapetest.Group(1), resolver)
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

// A plan is lowered for one group. A scope that names none (a required
// partition column unset) or that reads nothing is refused, so no lowered plan
// quietly carries no group predicate for a watch that needs one.
func TestLower_RefusesAScopeWithNoGroup(t *testing.T) {
	if len(coords.BuildLayout().Columns) > 0 {
		if _, err := Lower(`cat /log/foo.log`, coords.Set{{}}, noResolver{}); !errors.Is(err, coords.ErrRequiredUnset) {
			t.Errorf("Lower with an unset group = %v, want ErrRequiredUnset", err)
		}
	}
	if _, err := Lower(`cat /log/foo.log`, coords.Set{}, noResolver{}); !errors.Is(err, ErrNoSource) {
		t.Errorf("Lower with an empty scope = %v, want ErrNoSource", err)
	}
}

// A lowered plan keeps the field form of a stage that reads only some fields,
// through JSON, and its args are the plain query's, so a fire hydrates one set
// for both. Without it a cron's awk stage would be sent whole lines.
func TestLower_KeepsTheFieldForm(t *testing.T) {
	pipeline := `cat /log/weft/coordinator/main.log | awk '{ s += $3 } END { print s }'`
	lp, err := Lower(pipeline, shapetest.Group(1), noResolver{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	b, err := json.Marshal(lp)
	if err != nil {
		t.Fatal(err)
	}
	var got LoweredPlan
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	live, err := Plan(pipeline, shapetest.Group(1), "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if got.FieldSQL == "" || got.FieldSpec == nil || len(got.FieldSpec.Positions) != 1 || got.FieldSpec.Positions[0] != 3 {
		t.Fatalf("lowered field form = %q %+v, want one over $3", got.FieldSQL, got.FieldSpec)
	}
	// Same text as a live plan once the time bounds are out of it.
	if strings.Count(got.FieldSQL, "?") != strings.Count(got.DirectShardSQL, "?") {
		t.Errorf("field sql has %d placeholders, the plain sql %d", strings.Count(got.FieldSQL, "?"), strings.Count(got.DirectShardSQL, "?"))
	}
	if live.FieldSQL == "" {
		t.Errorf("the live plan has no field form either")
	}
}
