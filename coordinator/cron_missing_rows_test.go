package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/logdb"
)

// A cron whose log query is missing a shard's rows fails its run: no value
// is stored, and a script doesn't carry on to print one from what it had.
// Before, the query's error was a stderr line, so the run was recorded as
// an alert and the script's later output stored as its value.
func TestCron_MissingShardFailsTheRun(t *testing.T) {
	c := setupTestCoordinator(t)
	db := c.Store
	org, err := db.GetOrgByName("kevlar")
	if err != nil {
		t.Fatal(err)
	}
	e := &logdb.Shard{
		ID:          "shard-1",
		WindowStart: t1, WindowEnd: t2,
		State:     logdb.StateSuspended,
		Addr:      "10.0.0.9:9000",
		RowCount:  25,
		CreatedAt: time.Now(),
	}
	logdb.SeedShard(c.Provisioner, e)
	stampShardServicesFromApps(t, c, e, []string{"vimgym", "callbox"})

	ce := &CronExecutor{Store: db, Coord: c, RunTimeout: 5 * time.Second}
	for name, pipeline := range map[string]string{
		"count":  "cat vimgym.log | wc -l",
		"script": "cat vimgym.log | wc -l\necho 5",
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.SaveCronWatch(org.ID, "", name, "every 5m", pipeline, true, nil); err != nil {
				t.Fatal(err)
			}
			w, err := db.GetCronWatch(org.ID, name)
			if err != nil {
				t.Fatal(err)
			}
			ce.executeWatch(context.Background(), *w, fullRange)

			w, _ = db.GetCronWatch(org.ID, name)
			if w.LastStatus != "error" {
				t.Fatalf("status = %q, want error", w.LastStatus)
			}
			rows, err := db.ListCronResults(w.ID, 10, "")
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rows {
				if r.Value != nil || !strings.Contains(r.Stderr, "shard-1") {
					t.Fatalf("the failed run stored %+v; want no value and an error naming shard-1", r)
				}
			}
		})
	}
}
