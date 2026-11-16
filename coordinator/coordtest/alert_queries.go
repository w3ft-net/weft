package coordtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/coordinator"
)

// AlertQueries runs the suites of the alert stage reached through a query, as a
// user types `... | alert ...` at the shell, building a fresh Subject for each
// test.
func AlertQueries(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Subject)
	}{
		{"InteractiveAlertUsesCoordinatorNotifier", interactiveAlertUsesCoordinatorNotifier},
		{"InteractiveAlertFallbackWhenNoNotifier", interactiveAlertFallbackWhenNoNotifier},
		{"InteractiveAlertResolvesCallersAliases", interactiveAlertResolvesCallersAliases},
		{"InteractiveBinExpandResolvesCaller", interactiveBinExpandResolvesCaller},
		{"UpstreamFailureIsNotEvaluated", alertUpstreamFailureIsNotEvaluated},
		{"AbsentStillFiresOnEmptySuccess", alertAbsentStillFiresOnEmptySuccess},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newSubject(t)) })
	}
}

// run executes line as the subject's owner and returns what came out of it and
// the error that refused it, if any.
func (s Subject) run(t *testing.T, c *coordinator.Coordinator, line string) (text, stderr string, err error) {
	t.Helper()
	tr := coordinator.TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	res, err := c.Execute(context.Background(), line, tr, s.PrincipalOf(""))
	if err != nil {
		return "", "", err
	}
	d, err := res.Drain(context.Background())
	if err != nil {
		t.Fatalf("drain %q: %v", line, err)
	}
	return d.Text, d.Stderr, nil
}

func (s Subject) coordinator(t *testing.T, cfg coordinator.CoordinatorConfig) *coordinator.Coordinator {
	t.Helper()
	if cfg.Compute == nil {
		cfg.Compute = &compute.Engine{}
	}
	return s.NewCoordinator(t, cfg)
}

// With the coordinator's notifier set, interactive runs attempt real dispatch,
// so a missing-alias lookup surfaces the alias resolver's error, not the
// fallback's. (A cmd/coordinator that handed the notifier only to the cron
// executor once left every shell alert on the fallback.)
func interactiveAlertUsesCoordinatorNotifier(t *testing.T, s Subject) {
	c := s.coordinator(t, coordinator.CoordinatorConfig{Notifier: coordinator.NewNotifier(s.Notify)})

	// No alias is defined in /etc/notify.yaml, so Notifier.Send returns "no
	// notify.yaml configured". The point is that Send is reached.
	_, stderr, err := s.run(t, c, `echo 40 | alert above 30 --notify=foo`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(stderr, "notifier unavailable") {
		t.Errorf("fallback path hit — Notifier didn't propagate:\nstderr=%q", stderr)
	}
	if !strings.Contains(stderr, "dispatch to \"foo\" failed") {
		t.Errorf("expected dispatch-attempt error, got stderr=%q", stderr)
	}
}

// A coordinator without a notifier still runs alerts, just without dispatch.
// "would dispatch" is the documented fallback.
func interactiveAlertFallbackWhenNoNotifier(t *testing.T, s Subject) {
	c := s.coordinator(t, coordinator.CoordinatorConfig{})
	_, stderr, err := s.run(t, c, `echo 40 | alert above 30 --notify=foo`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(stderr, "notifier unavailable") {
		t.Errorf("expected fallback message, got stderr=%q", stderr)
	}
}

// noCredsSlack stands in for the hosted slack target, which the core does not
// ship: it fails the way that target does for an org that never connected Slack.
type noCredsSlack struct{}

func (noCredsSlack) Kind() string { return "slack" }
func (noCredsSlack) Open(e *yaml.Node) (alerting.Destination, error) {
	return alerting.Destination{Key: "slack:" + e.Value, Name: e.Value, Send: func(context.Context, alerting.Alert) error {
		return errors.New("no Slack credentials configured")
	}}, nil
}

// An interactive alert dispatches as the caller's owner: it finds the aliases
// the owner saved. If it looked under another owner it would stop at "no
// notify.yaml configured"; reaching the Slack target and failing at its
// credentials proves the alias was found.
func interactiveAlertResolvesCallersAliases(t *testing.T, s Subject) {
	err := s.Notify.Save(s.Owner, "", "system:\n  email:\n    - \"oncall@example.test\"\nfoo:\n  slack:\n    - \"#oncall\"\n")
	if err != nil {
		t.Fatalf("save notify aliases: %v", err)
	}
	c := s.coordinator(t, coordinator.CoordinatorConfig{Notifier: coordinator.NewNotifier(s.Notify, noCredsSlack{})})

	_, stderr, err := s.run(t, c, `echo 40 | alert above 30 --notify=foo`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(stderr, "no notify.yaml configured") {
		t.Errorf("notifier used the wrong owner (got config-not-found):\n  stderr=%q", stderr)
	}
	if !strings.Contains(stderr, "no Slack credentials configured") {
		t.Errorf("expected to reach Slack-credentials check, got stderr=%q", stderr)
	}
}

// A /bin script typed at the shell resolves through the caller's scripts, and
// its body is inlined with the positionals bound.
func interactiveBinExpandResolvesCaller(t *testing.T, s Subject) {
	if err := s.Bins.Save(s.Owner, "", "echo_first", "echo $1", "bash"); err != nil {
		t.Fatalf("save script: %v", err)
	}
	c := s.coordinator(t, coordinator.CoordinatorConfig{Notifier: coordinator.NewNotifier(s.Notify)})

	text, stderr, err := s.run(t, c, `echo_first disk`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// If /bin resolution is broken at the owner boundary, warp emits
	// "echo_first: command not found".
	if strings.Contains(stderr, "command not found") {
		t.Errorf("/bin lookup broken for the caller:\n  stderr=%q", stderr)
	}
	if got := strings.TrimSpace(text); got != "disk" {
		t.Errorf("echo_first disk = %q, want disk (stderr=%q)", got, stderr)
	}
}

// `alert absent` fires on empty input. Empty input from a query that FAILED
// (unknown source, bad pipeline) is not absence: the alert must refuse to
// evaluate rather than page on a broken query, and the error must say so (a cron
// run then records status "error", opens no incident).
func alertUpstreamFailureIsNotEvaluated(t *testing.T, s Subject) {
	c := s.coordinator(t, coordinator.CoordinatorConfig{Notifier: coordinator.NewNotifier(s.Notify)})
	for _, cond := range []string{"absent", "below 5", "changed", "any"} {
		t.Run(cond, func(t *testing.T) {
			text, stderr, err := s.run(t, c, `cat /log/ghost/prod/api/main.log | alert `+cond)
			if err != nil {
				text = err.Error()
			} else {
				text += stderr
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
func alertAbsentStillFiresOnEmptySuccess(t *testing.T, s Subject) {
	c := s.coordinator(t, coordinator.CoordinatorConfig{Notifier: coordinator.NewNotifier(s.Notify)})
	_, stderr, err := s.run(t, c, `echo hello | grep nomatch | alert absent`)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(stderr, "absent") {
		t.Errorf("empty-but-successful pipeline should fire absent, stderr=%q", stderr)
	}
}
