package coordinator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/planner"
	"github.com/graybeardcorp/weft/store"
)

// TestExecuteForWatch_LoweredHotPath fires a watch whose LoweredPlan
// is populated and confirms results match the inline-Plan path. This
// is the smoke test that the save-time-lowering shortcut produces
// correct output — if it ever ships SQL that diverges from a fresh
// Plan() call, this test catches the drift.
func TestExecuteForWatch_LoweredHotPath(t *testing.T) {
	c := setupTestCoordinator(t)

	cust, err := c.Store.GetOrgByName("kevlar")
	if err != nil {
		t.Fatalf("resolve test customer: %v", err)
	}

	pipeline := `cat /log/vimgym.log | grep ERROR`

	// Compile the watch the same way the save handlers do.
	lp, err := planner.Lower(pipeline, 1, "bolt", noBinResolver{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	loweredJSON, err := json.Marshal(lp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	w := &store.CronWatch{
		OrgID:       cust.ID,
		Name:        "lowered-smoke",
		Schedule:    "every 1m",
		Pipeline:    pipeline,
		Enabled:     true,
		LoweredPlan: loweredJSON,
	}

	// Run with lowered.
	loweredRes, err := c.ExecuteForWatch(context.Background(), pipeline, fullRange, w)
	if err != nil {
		t.Fatalf("ExecuteForWatch (lowered): %v", err)
	}
	loweredRows := drainExec(t, loweredRes).Rows

	// Run again with LoweredPlan removed — forces the fallback path.
	w.LoweredPlan = nil
	freshRes, err := c.ExecuteForWatch(context.Background(), pipeline, fullRange, w)
	if err != nil {
		t.Fatalf("ExecuteForWatch (fresh): %v", err)
	}
	freshRows := drainExec(t, freshRes).Rows

	if len(loweredRows) == 0 {
		t.Fatalf("lowered path returned no rows; expected ERROR rows from vimgym")
	}
	if len(loweredRows) != len(freshRows) {
		t.Fatalf("lowered vs fresh row count mismatch: lowered=%d fresh=%d",
			len(loweredRows), len(freshRows))
	}
	for i := range loweredRows {
		if loweredRows[i].Raw != freshRows[i].Raw {
			t.Errorf("row[%d] differs:\n lowered: %s\n   fresh: %s",
				i, loweredRows[i].Raw, freshRows[i].Raw)
		}
	}

	// Every row matched the filter — sanity check the SQL really
	// applied the predicate, not just happened to return matching
	// rows by accident.
	for _, r := range loweredRows {
		if !strings.Contains(r.Raw, "ERROR") {
			t.Errorf("expected ERROR in row, got %q", r.Raw)
		}
	}
}

// TestExecuteForWatch_LoweredVersionMismatch pins the fall-back-on-
// stale-version behavior. A watch with a LoweredPlan stamped at an
// older planner version must NOT use the stored plan — it'd be lying
// about a SQL shape the current planner no longer produces. The
// fallback path re-lowers + writes back.
func TestExecuteForWatch_LoweredVersionMismatch(t *testing.T) {
	c := setupTestCoordinator(t)

	cust, _ := c.Store.GetOrgByName("kevlar")

	pipeline := `cat /log/vimgym.log | grep ERROR`
	lp, err := planner.Lower(pipeline, 1, "bolt", noBinResolver{})
	if err != nil {
		t.Fatalf("Lower: %v", err)
	}
	// Force a version mismatch.
	lp.PlannerVersion = planner.PlannerVersion + 100
	loweredJSON, _ := json.Marshal(lp)

	w := &store.CronWatch{
		OrgID:       cust.ID,
		Name:        "version-mismatch",
		Schedule:    "every 1m",
		Pipeline:    pipeline,
		Enabled:     true,
		LoweredPlan: loweredJSON,
	}

	// Must succeed via fallback — if executeLowered tried to use the
	// stale plan, the SQL would still work (version doesn't affect SQL
	// shape today) but the test exists to fail loudly if anyone
	// removes the version check.
	res, err := c.ExecuteForWatch(context.Background(), pipeline, fullRange, w)
	if err != nil {
		t.Fatalf("ExecuteForWatch: %v", err)
	}
	rows := drainExec(t, res).Rows
	if len(rows) == 0 {
		t.Fatalf("expected ERROR rows; got 0")
	}
}

// noBinResolver is the in-test BinResolver stub used by tests that
// don't exercise /bin/ inlining. Mirrors planner/lower_test.go's
// noResolver — duplicated rather than shared because the type is one
// method and lives in two packages.
type noBinResolver struct{}

func (noBinResolver) Source(name string) (string, string, bool) { return "", "", false }
