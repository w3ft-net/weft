package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
)

// `alert absent` fires on empty input. Empty input from a query that
// FAILED (unknown source, bad pipeline) is not absence: the alert must
// refuse to evaluate rather than page on a broken query, and the error
// must say so (a cron run then records status "error", opens no incident).
func TestAlert_UpstreamFailureIsNotEvaluated(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	cust, err := db.GetOrgByID(custID)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCoordinator(CoordinatorConfig{Pool: &compute.LocalComputePool{}, Store: db, Notifier: NewNotifier(db, nil)})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	for _, cond := range []string{"absent", "below 5", "changed", "any"} {
		t.Run(cond, func(t *testing.T) {
			res, err := c.Execute(context.Background(), `cat /log/ghost/prod/api/main.log | alert `+cond, tr, cust.Name, "")
			var text string
			if err == nil {
				d := drainExec(t, res)
				text = d.Text + d.Stderr
			} else {
				text = err.Error()
			}
			if err == nil || !strings.Contains(err.Error(), "upstream") {
				t.Errorf("alert %s over a failed query: want an \"upstream failed\" error, got err=%v text=%q", cond, err, text)
			}
			if strings.Contains(text, "absent for") || strings.Contains(text, "non-empty output") {
				t.Errorf("alert %s fired on a failed query: %q", cond, text)
			}
		})
	}
}

// Genuine silence (the source ran and returned nothing) is still absence.
func TestAlert_AbsentStillFiresOnEmptySuccess(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	cust, err := db.GetOrgByID(custID)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCoordinator(CoordinatorConfig{Pool: &compute.LocalComputePool{}, Store: db, Notifier: NewNotifier(db, nil)})
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}

	res, err := c.Execute(context.Background(), `echo hello | grep nomatch | alert absent`, tr, cust.Name, "")
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	d := drainExec(t, res)
	if !strings.Contains(d.Stderr, "absent") {
		t.Errorf("empty-but-successful pipeline should fire absent, stderr=%q", d.Stderr)
	}
}
