package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// runCronAlert evaluates an alert stage the way a cron tick does: the
// watch's earlier runs are in the store and the current run's output is
// not yet persisted (persistResults runs after the pipeline).
func runCronAlert(t *testing.T, db *store.Store, custID store.OrgID, watchName, args, stdin string) string {
	t.Helper()
	w, err := db.GetCronWatch(custID, opsNS(t, db, custID), watchName)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr strings.Builder
	AlertCommand(context.Background(), strings.Fields(args), strings.NewReader(stdin), &stdout, &stderr,
		&AlertContext{Store: db, Watch: w, OrgID: custID})
	return stderr.String()
}

func TestAlert_RisingComparesAgainstPreviousTick(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	seedWatch(t, db, custID, "rising-prev", []string{"10", "100"}, time.Minute)

	// The previous tick printed 100; 110 is a 10% rise, under the 50% bar.
	if stderr := runCronAlert(t, db, custID, "rising-prev", "rising 50%", "110\n"); stderr != "" {
		t.Errorf("110 after 100 fired rising 50%%: %q", stderr)
	}
}

func TestAlert_ChangedMultiRowComparesValues(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	if err := db.SaveCronWatch(custID, opsNS(t, db, custID), "", "changed-rows", "every 1m", "echo test", true, nil); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, opsNS(t, db, custID), "changed-rows")
	if err != nil {
		t.Fatal(err)
	}
	five := 5.0
	now := time.Now()
	for i := 2; i >= 1; i-- {
		if err := db.SaveCronResults([]store.CronResult{{
			CronWatchID: w.ID, RanAt: now.Add(-time.Duration(i) * time.Minute),
			Stdout: "a 5", Value: &five, Tag: "a",
		}}); err != nil {
			t.Fatal(err)
		}
	}

	// Tag a printed 5 on every earlier tick and prints 5 again.
	if stderr := runCronAlert(t, db, custID, "changed-rows", "changed -v 2 -t 1", "a 5\n"); stderr != "" {
		t.Errorf("unchanged value fired changed: %q", stderr)
	}
}
