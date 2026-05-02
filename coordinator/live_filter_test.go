package coordinator

import (
	"testing"

	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"time"
)

// TestLiveFilter_CutAppliesToLiveRow pins the bug we just hit running
// the demo: `tail -f <log> | cut -d' ' -f3` shows the cut applied to
// the initial DuckDB read but emits whole raw lines for everything the
// broker pushes after that. The compute pipeline runs only on the
// historical batch; live rows skip it entirely.
//
// The fix is the LiveFilter closure: BuildLiveFilter takes a parsed
// QueryPlan and returns a per-row function that re-runs the user's
// pipeline stages against the raw line. This test pins the contract —
// passing rows return cut output, not the original Raw — so the wiring
// in coordinator/service/server.go can call the closure instead of
// blindly forwarding row.Raw.
func TestLiveFilter_CutAppliesToLiveRow(t *testing.T) {
	t.Helper()
	runFilterCase(t, `tail -f vimgym.log | cut -d' ' -f3`,
		&pb.LogRow{App: "vimgym", Raw: "2026-03-28T12:00:00 INFO vimgym i-001 request completed"},
		[]string{"vimgym"})
}

// TestLiveFilter_GrepFiltersLiveRow: a non-matching row should drop,
// a matching row should pass through unchanged.
func TestLiveFilter_GrepFiltersLiveRow(t *testing.T) {
	pipeline := `tail -f vimgym.log | grep ERROR`
	runFilterCase(t, pipeline,
		&pb.LogRow{App: "vimgym", Raw: "2026-03-28T12:00:00 ERROR boom"},
		[]string{"2026-03-28T12:00:00 ERROR boom"})
	runFilterCase(t, pipeline,
		&pb.LogRow{App: "vimgym", Raw: "2026-03-28T12:00:00 INFO healthy"},
		nil)
}

// TestLiveFilter_SedSubstitution: sed s/// transforms must apply per
// live row, otherwise the live tail and historical tail render
// differently for the same query.
func TestLiveFilter_SedSubstitution(t *testing.T) {
	runFilterCase(t, `tail -f vimgym.log | sed 's/vimgym/APP/g'`,
		&pb.LogRow{App: "vimgym", Raw: "2026-03-28T12:00:00 INFO vimgym i-001 request"},
		[]string{"2026-03-28T12:00:00 INFO APP i-001 request"})
}

// TestLiveFilter_Chain runs grep + sed + cut in sequence — pins that
// stages compose on the live path, not just one at a time.
func TestLiveFilter_Chain(t *testing.T) {
	pipeline := `tail -f vimgym.log | grep INFO | sed 's/vimgym/APP/g' | cut -d' ' -f1-3`
	runFilterCase(t, pipeline,
		&pb.LogRow{App: "vimgym", Raw: "2026-03-28T12:00:00 INFO vimgym i-001 request"},
		[]string{"2026-03-28T12:00:00 INFO APP"})
	runFilterCase(t, pipeline,
		&pb.LogRow{App: "vimgym", Raw: "2026-03-28T12:00:00 ERROR vimgym i-001 boom"},
		nil)
}

// TestLiveFilter_AppGateNamespaceMatchesSubApp: a bare-namespace source
// (`tail -f acme.log`) should match rows for any sub-app under it,
// matching the SQL predicate the planner emits for the historical path.
func TestLiveFilter_AppGateNamespaceMatchesSubApp(t *testing.T) {
	pipeline := `tail -f acme.log | grep ERROR`
	runFilterCase(t, pipeline,
		&pb.LogRow{App: "acme/api", Raw: "ERROR from acme/api"},
		[]string{"ERROR from acme/api"})
	runFilterCase(t, pipeline,
		&pb.LogRow{App: "globex/api", Raw: "ERROR from globex/api"},
		nil)
}

func runFilterCase(t *testing.T, pipeline string, row *pb.LogRow, want []string) {
	t.Helper()
	plan, err := planner.Plan(pipeline, "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Plan(%q): %v", pipeline, err)
	}
	filter := BuildLiveFilter(plan)
	got := filter(row)
	if len(got) != len(want) {
		t.Fatalf("filter(%q on %q):\n  got:  %v\n  want: %v",
			pipeline, row.Raw, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("filter(%q on %q) line %d:\n  got:  %q\n  want: %q",
				pipeline, row.Raw, i, got[i], want[i])
		}
	}
}

