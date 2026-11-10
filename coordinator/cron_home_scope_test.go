package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/planner"
	"github.com/graybeardcorp/weft/store"
)

// A cron runs with its home namespace's authority. A /bin script it calls can
// be edited after the cron was saved, so the save-time check can't be what
// keeps it in its namespace: the fire must refuse the namespace it isn't in.
func TestExecuteForWatch_CronReadsOnlyItsHomeNamespace(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	alpha, err := db.FindOrCreateNamespace(custID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FindOrCreateNamespace(custID, "beta"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveShellFunction(custID, "", "scan", "cat /log/alpha/api/main.log", "bash"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(custID, alpha.ID, "", "errors", "every 5m", "scan", true, nil); err != nil {
		t.Fatal(err)
	}
	// Someone with no cron in mind edits the script to read another namespace.
	if err := db.SaveShellFunction(custID, "", "scan", "cat /log/beta/api/main.log", "bash"); err != nil {
		t.Fatal(err)
	}
	w, err := db.GetCronWatch(custID, alpha.ID, "errors")
	if err != nil {
		t.Fatal(err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	res, err := c.ExecuteForWatch(context.Background(), strings.TrimSpace(w.Pipeline), tr, w)
	if err == nil && res != nil {
		d, derr := res.Drain(context.Background())
		if derr != nil {
			err = derr
		} else if d.Stderr != "" {
			err = errors.New(d.Stderr)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "unknown source: beta") {
		t.Fatalf("err = %v, want the cron refused for reading beta", err)
	}
}

// The lowered fast path skips planning, so it needs its own check: a stored
// plan that reads beta does not run for a cron homed in alpha.
func TestExecuteForWatch_LoweredPlanOutsideTheHomeIsRefused(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	alpha, _ := db.FindOrCreateNamespace(custID, "alpha")
	db.FindOrCreateNamespace(custID, "beta")

	pipeline := "cat /log/beta/api/main.log | grep ERROR"
	lp, err := planner.Lower(pipeline, int64(custID), "bolt", noBinResolver{})
	if err != nil {
		t.Fatal(err)
	}
	lowered, err := json.Marshal(lp)
	if err != nil {
		t.Fatal(err)
	}
	w := &store.CronWatch{OrgID: custID, NamespaceID: &alpha.ID, Name: "leak", Schedule: "every 1m", Pipeline: pipeline, Enabled: true, LoweredPlan: lowered}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	res, err := c.ExecuteForWatch(context.Background(), pipeline, tr, w)
	if err == nil && res != nil {
		d, derr := res.Drain(context.Background())
		if derr != nil {
			err = derr
		} else if d.Stderr != "" {
			err = errors.New(d.Stderr)
		}
	}
	if err == nil || !strings.Contains(err.Error(), "unknown source: beta") {
		t.Fatalf("err = %v, want the stored plan refused for reading beta", err)
	}
}
