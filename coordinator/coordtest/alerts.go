package coordtest

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/shape"
)

func formatValue(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// Alerts runs the `alert` command's suites, building a fresh Subject for each
// test.
func Alerts(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Subject)
	}{
		{"RisingComparesAgainstPreviousTick", alertRisingComparesAgainstPreviousTick},
		{"ChangedMultiRowComparesValues", alertChangedMultiRowComparesValues},
		{"EditingTheAlertStageKeepsTheIncident", incidentEditingTheAlertStageKeepsTheIncident},
		{"EditedConditionStillClosesTheIncident", incidentEditedConditionStillClosesTheIncident},
		{"IfZeroRepagesAfterClearing", incidentIfZeroRepagesAfterClearing},
		{"FailedDispatchIsRetriedUntilDelivered", incidentFailedDispatchIsRetriedUntilDelivered},
		{"RetryReachesOnlyTheTargetsThatMissed", incidentRetryReachesOnlyTheTargetsThatMissed},
		{"TagsAreIndependent", incidentTagsAreIndependent},
		{"DispatchNamesCronByPath", alertDispatchNamesCronByPath},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newSubject(t)) })
	}
}

// runCronAlert evaluates an alert stage the way a cron tick does: the watch's
// earlier runs are in the store and the current run's output is not yet
// persisted (persistResults runs after the pipeline).
func runCronAlert(t *testing.T, s Subject, w cron.Watch, args, stdin string) string {
	t.Helper()
	var stdout, stderr strings.Builder
	coordinator.AlertCommand(context.Background(), strings.Fields(args), strings.NewReader(stdin), &stdout, &stderr,
		&coordinator.AlertContext{Crons: s.Crons, Sources: s.Sources, Watch: &w, Owner: s.Owner})
	return stderr.String()
}

func alertRisingComparesAgainstPreviousTick(t *testing.T, s Subject) {
	w := s.saveWatch(t, "ops", "rising-prev")
	s.record(t, w, "", 10, 100)

	// The previous tick printed 100; 110 is a 10% rise, under the 50% bar.
	if stderr := runCronAlert(t, s, w, "rising 50%", "110\n"); stderr != "" {
		t.Errorf("110 after 100 fired rising 50%%: %q", stderr)
	}
}

func alertChangedMultiRowComparesValues(t *testing.T, s Subject) {
	w := s.saveWatch(t, "ops", "changed-rows")
	s.record(t, w, "a", 5, 5)

	// Tag a printed 5 on every earlier tick and prints 5 again.
	if stderr := runCronAlert(t, s, w, "changed -v 2 -t 1", "a 5\n"); stderr != "" {
		t.Errorf("unchanged value fired changed: %q", stderr)
	}
}

// incidentRig runs `alert` as a cron would: with a saved watch and a store, so
// incidents are opened and closed, and with a notifier that records sends.
type incidentRig struct {
	t     *testing.T
	ac    *coordinator.AlertContext
	notif *scriptedNotifier
}

// scriptedNotifier stands in for an alias with several targets. It skips the
// ones already delivered, as Notifier.Send does, fails the ones listed in down,
// and counts every attempt per target.
type scriptedNotifier struct {
	targets  []string
	down     map[string]bool
	attempts map[string]int
}

func (n *scriptedNotifier) Send(_ context.Context, _ shape.Added, _, _, _ string, delivered map[string]bool) ([]string, error) {
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

func newIncidentRig(t *testing.T, s Subject) *incidentRig {
	t.Helper()
	w := s.saveWatch(t, "ops", "error-rate")
	n := &scriptedNotifier{targets: []string{"pagerduty:ops"}, down: map[string]bool{}, attempts: map[string]int{}}
	return &incidentRig{t: t, notif: n, ac: &coordinator.AlertContext{Watch: &w, Crons: s.Crons, Notifier: n, Owner: s.Owner}}
}

// tick runs one cron tick: stdin piped into `alert <args>`. It returns the exit code.
func (r *incidentRig) tick(stdin string, args ...string) int {
	r.t.Helper()
	var out, errBuf strings.Builder
	return coordinator.AlertCommand(context.Background(), args, strings.NewReader(stdin), &out, &errBuf, r.ac)
}

// An incident belongs to the cron, not to the text of its alert stage. Raising
// the threshold (or changing the alias or message) while the condition still
// holds must not page a second time.
func incidentEditingTheAlertStageKeepsTheIncident(t *testing.T, s Subject) {
	r := newIncidentRig(t, s)
	r.tick("70\n", "above", "50", "--notify=ops")
	r.tick("70\n", "above", "60", "--notify=ops")
	if r.notif.total() != 1 {
		t.Errorf("sends = %d, want 1: editing the threshold mid-incident re-paged", r.notif.total())
	}
}

// ...and the incident must still close when the edited condition clears.
func incidentEditedConditionStillClosesTheIncident(t *testing.T, s Subject) {
	r := newIncidentRig(t, s)
	r.tick("70\n", "above", "50", "--notify=ops")
	r.tick("10\n", "above", "60", "--notify=ops") // cleared under the new threshold
	r.tick("70\n", "above", "50", "--notify=ops") // trips again under the old threshold: a new incident
	if r.notif.total() != 2 {
		t.Errorf("sends = %d, want 2: the incident opened under the old threshold never closed", r.notif.total())
	}
}

// --if-zero "$?" carries the predicate's exit code in its arguments, which
// differ between a firing tick ("0") and a clear one ("1"). The incident has to
// close on the clear tick and re-open when the predicate trips again.
func incidentIfZeroRepagesAfterClearing(t *testing.T, s Subject) {
	r := newIncidentRig(t, s)
	r.tick("", "--if-zero", "0", "--notify=ops") // trips: page
	r.tick("", "--if-zero", "1", "--notify=ops") // clears
	r.tick("", "--if-zero", "0", "--notify=ops") // trips again: page
	if r.notif.total() != 2 {
		t.Errorf("sends = %d, want 2: --if-zero never re-paged after clearing", r.notif.total())
	}
}

// A failed send leaves nobody notified, so the next tick has to try again. Once
// it gets through, later ticks in the same incident stay quiet.
func incidentFailedDispatchIsRetriedUntilDelivered(t *testing.T, s Subject) {
	r := newIncidentRig(t, s)
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
// retry goes only to that one: the target that already heard about the incident
// is not told again on every tick until the other recovers.
func incidentRetryReachesOnlyTheTargetsThatMissed(t *testing.T, s Subject) {
	r := newIncidentRig(t, s)
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

// Each tag is its own incident, so one host's open incident neither suppresses
// nor closes another's.
func incidentTagsAreIndependent(t *testing.T, s Subject) {
	r := newIncidentRig(t, s)
	args := []string{"above", "5", "-v", "1", "-t", "2", "--notify=ops"}
	r.tick("7 disk\n7 cpu\n", args...) // both open
	r.tick("7 disk\n3 cpu\n", args...) // cpu clears
	r.tick("7 disk\n7 cpu\n", args...) // cpu trips again; disk stays quiet
	if got := r.notif.attempts["pagerduty:ops"]; got != 3 {
		t.Errorf("sends = %d, want 3 (disk once, cpu twice)", got)
	}
}

// recordingNotifier captures the name handed to Send.
type recordingNotifier struct{ watches []string }

func (r *recordingNotifier) Send(_ context.Context, _ shape.Added, _, watch, _ string, _ map[string]bool) ([]string, error) {
	r.watches = append(r.watches, watch)
	return nil, nil
}

// Two namespaces can each hold a cron called `disk-use`. The name handed to the
// notifier builds the PagerDuty dedup key and the webhook `cron` field, so it
// must be <namespace>/<name> or the two crons' incidents collapse into one.
func alertDispatchNamesCronByPath(t *testing.T, s Subject) {
	var names []string
	for _, ns := range []string{"web", "api"} {
		w := s.saveWatch(t, ns, "disk-use")
		rec := &recordingNotifier{}
		ac := &coordinator.AlertContext{Crons: s.Crons, Sources: s.Sources, Notifier: rec, Owner: s.Owner, Watch: &w}
		var stdout, stderr strings.Builder
		coordinator.AlertCommand(context.Background(), []string{"above", "30", "--notify=foo"}, strings.NewReader("40\n"), &stdout, &stderr, ac)
		if len(rec.watches) != 1 {
			t.Fatalf("%s: want 1 dispatch, got %d", ns, len(rec.watches))
		}
		names = append(names, rec.watches[0])
	}
	if names[0] != "web/disk-use" || names[1] != "api/disk-use" {
		t.Fatalf("dispatched names %v, want [web/disk-use api/disk-use]", names)
	}
}
