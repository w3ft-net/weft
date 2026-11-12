//go:build !defaultshape

package coordinator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// incidentRig runs `alert` as a cron would: with a saved watch and a store,
// so incidents are opened and closed, and with a notifier that records sends.
type incidentRig struct {
	t     *testing.T
	ac    *AlertContext
	notif *scriptedNotifier
}

// scriptedNotifier stands in for an alias with several targets. It skips the
// ones already delivered, as Notifier.Send does, fails the ones listed in
// down, and counts every attempt per target.
type scriptedNotifier struct {
	targets  []string
	down     map[string]bool
	attempts map[string]int
}

func (n *scriptedNotifier) Send(_ context.Context, _ store.OrgID, _, _, _ string, delivered map[string]bool) ([]string, error) {
	var sent []string
	var firstErr error
	for _, t := range n.targets {
		if delivered[t] {
			continue
		}
		n.attempts[t]++
		if n.down[t] {
			if firstErr == nil {
				firstErr = errors.New(t + " unreachable")
			}
			continue
		}
		sent = append(sent, t)
	}
	return sent, firstErr
}

// total is how many sends went out across every target.
func (n *scriptedNotifier) total() int {
	sum := 0
	for _, c := range n.attempts {
		sum += c
	}
	return sum
}

func newIncidentRig(t *testing.T) *incidentRig {
	t.Helper()
	db, orgID := testStoreAndCustomer(t)
	w := &store.CronWatch{OrgID: orgID, Name: "error-rate", Schedule: "every 1m", Pipeline: "true", Enabled: true}
	if err := db.DB().WithContext(store.WithOrg(context.Background(), orgID)).Create(w).Error; err != nil {
		t.Fatal(err)
	}
	n := &scriptedNotifier{targets: []string{"pagerduty:ops"}, down: map[string]bool{}, attempts: map[string]int{}}
	return &incidentRig{t: t, notif: n, ac: &AlertContext{Watch: w, Store: db, Notifier: n, OrgID: orgID}}
}

// tick runs one cron tick: stdin piped into `alert <args>`. It returns the exit code.
func (r *incidentRig) tick(stdin string, args ...string) int {
	r.t.Helper()
	var out, errBuf strings.Builder
	return AlertCommand(store.WithOrg(context.Background(), r.ac.OrgID), args, strings.NewReader(stdin), &out, &errBuf, r.ac)
}

// An incident belongs to the cron, not to the text of its alert stage.
// Raising the threshold (or changing the alias or message) while the
// condition still holds must not page a second time.
func TestAlertIncident_EditingTheAlertStageKeepsTheIncident(t *testing.T) {
	r := newIncidentRig(t)
	r.tick("70\n", "above", "50", "--notify=ops")
	r.tick("70\n", "above", "60", "--notify=ops")
	if r.notif.total() != 1 {
		t.Errorf("sends = %d, want 1: editing the threshold mid-incident re-paged", r.notif.total())
	}
}

// ...and the incident must still close when the edited condition clears.
func TestAlertIncident_EditedConditionStillClosesTheIncident(t *testing.T) {
	r := newIncidentRig(t)
	r.tick("70\n", "above", "50", "--notify=ops")
	r.tick("10\n", "above", "60", "--notify=ops") // cleared under the new threshold
	r.tick("70\n", "above", "50", "--notify=ops") // trips again under the old threshold: a new incident
	if r.notif.total() != 2 {
		t.Errorf("sends = %d, want 2: the incident opened under the old threshold never closed", r.notif.total())
	}
}

// --if-zero "$?" carries the predicate's exit code in its arguments, which
// differ between a firing tick ("0") and a clear one ("1"). The incident has
// to close on the clear tick and re-open when the predicate trips again.
func TestAlertIncident_IfZeroRepagesAfterClearing(t *testing.T) {
	r := newIncidentRig(t)
	r.tick("", "--if-zero", "0", "--notify=ops") // trips: page
	r.tick("", "--if-zero", "1", "--notify=ops") // clears
	r.tick("", "--if-zero", "0", "--notify=ops") // trips again: page
	if r.notif.total() != 2 {
		t.Errorf("sends = %d, want 2: --if-zero never re-paged after clearing", r.notif.total())
	}
}

// A failed send leaves nobody notified, so the next tick has to try again.
// Once it gets through, later ticks in the same incident stay quiet.
func TestAlertIncident_FailedDispatchIsRetriedUntilDelivered(t *testing.T) {
	r := newIncidentRig(t)
	r.notif.down["pagerduty:ops"] = true

	if code := r.tick("70\n", "above", "50", "--notify=ops"); code == 0 {
		t.Error("tick 1: a failed dispatch should exit non-zero")
	}
	delete(r.notif.down, "pagerduty:ops") // PagerDuty comes back
	if code := r.tick("70\n", "above", "50", "--notify=ops"); code != 0 {
		t.Errorf("tick 2: exit %d, want the retry to succeed", code)
	}
	r.tick("70\n", "above", "50", "--notify=ops")
	if r.notif.total() != 2 {
		t.Errorf("sends = %d, want 2 (the failure, one retry, then quiet)", r.notif.total())
	}
}

// Delivery is tracked per target. When one of an alias's targets is down, the
// retry goes only to that one: the target that already heard about the
// incident is not told again on every tick until the other recovers.
func TestAlertIncident_RetryReachesOnlyTheTargetsThatMissed(t *testing.T) {
	r := newIncidentRig(t)
	r.notif.targets = []string{"slack:#alerts", "pagerduty:ops"}
	r.notif.down["pagerduty:ops"] = true

	r.tick("70\n", "above", "50", "--notify=ops")
	r.tick("70\n", "above", "50", "--notify=ops")
	delete(r.notif.down, "pagerduty:ops")
	r.tick("70\n", "above", "50", "--notify=ops")
	r.tick("70\n", "above", "50", "--notify=ops")

	if got := r.notif.attempts["slack:#alerts"]; got != 1 {
		t.Errorf("slack attempts = %d, want 1: it was told on the first tick and never again", got)
	}
	if got := r.notif.attempts["pagerduty:ops"]; got != 3 {
		t.Errorf("pagerduty attempts = %d, want 3 (two failures, then delivered)", got)
	}
}

// Each tag is its own incident, so one host's open incident neither
// suppresses nor closes another's.
func TestAlertIncident_TagsAreIndependent(t *testing.T) {
	r := newIncidentRig(t)
	args := []string{"above", "5", "-v", "1", "-t", "2", "--notify=ops"}
	r.tick("7 disk\n7 cpu\n", args...) // both open
	r.tick("7 disk\n3 cpu\n", args...) // cpu clears
	r.tick("7 disk\n7 cpu\n", args...) // cpu trips again; disk stays quiet
	if got := r.notif.attempts["pagerduty:ops"]; got != 3 {
		t.Errorf("sends = %d, want 3 (disk once, cpu twice)", got)
	}
}
