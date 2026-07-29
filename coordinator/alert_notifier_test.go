package coordinator

import (
	"strings"
	"testing"
	"time"
)

// TestQuery_InteractiveAlertUsesCoordinatorNotifier — regression guard for
// the bug where cmd/coordinator handed NewNotifier only to CronExecutor,
// leaving Coordinator.Notifier nil. Shell alerts (`| alert --notify=foo`)
// then fell through to the "notifier unavailable — would dispatch"
// fallback regardless of whether an alias was configured.
//
// With the fix, c.Notifier is set and interactive runs attempt real
// dispatch — so a missing-alias lookup surfaces the alias resolver's
// error, not the fallback's.
func TestQuery_InteractiveAlertUsesCoordinatorNotifier(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	_ = custID

	cust, err := db.GetOrgByID(custID)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}

	c := NewCoordinator(CoordinatorConfig{
		Store:    db,
		Notifier: NewNotifier(db, nil),
	})

	// No alias is defined in /etc/notify.yaml, so Notifier.Send returns
	// "no notify.yaml configured". The point is that Send is REACHED —
	// meaning Coordinator.Notifier propagated into the interactive
	// AlertContext.
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	result, err := c.Execute(`echo 40 | alert above 30 --notify=foo`, tr, cust.Name, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	resultD := drainExec(t, result)

	if strings.Contains(resultD.Stderr, "notifier unavailable") {
		t.Errorf("fallback path hit — Notifier didn't propagate:\nstderr=%q", resultD.Stderr)
	}
	if !strings.Contains(resultD.Stderr, "dispatch to \"foo\" failed") {
		t.Errorf("expected dispatch-attempt error, got stderr=%q", resultD.Stderr)
	}
}

// TestQuery_InteractiveAlertFallbackWhenNoNotifier — companion to the above.
// A Coordinator without a Notifier still runs alerts, just without
// dispatch. The "would dispatch" message is the documented fallback.
func TestQuery_InteractiveAlertFallbackWhenNoNotifier(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	cust, err := db.GetOrgByID(custID)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}

	c := NewCoordinator(CoordinatorConfig{
		Store: db,
		// Notifier: nil
	})

	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	result, err := c.Execute(`echo 40 | alert above 30 --notify=foo`, tr, cust.Name, "")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	resultD := drainExec(t, result)

	if !strings.Contains(resultD.Stderr, "notifier unavailable") {
		t.Errorf("expected fallback message, got stderr=%q", resultD.Stderr)
	}
}
