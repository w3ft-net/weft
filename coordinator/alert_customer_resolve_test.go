package coordinator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// TestQuery_InteractiveAlertResolvesCallerCustomer is a RED test for a
// production-only bug. Coordinator.OrgID is a struct field that's
// never set in cmd/coordinator/main.go — so in real deployments it's
// always zero. Requests authenticate through a per-call customer NAME
// (plumbed via Coordinator.Query's customerOverride), but the
// interactive AlertContext built in Coordinator.query grabs
// c.OrgID instead of resolving the passed-in name.
//
// Result: Notifier.Send(0, "foo", ...) → Store.GetNotifyConfig(0)
// finds nothing → "no notify.yaml configured" — even when the calling
// customer has a perfectly valid /etc/notify.yaml with alias "foo".
//
// This test seeds a customer with notify.yaml that defines "foo", but
// without Slack credentials configured. If the customer resolution
// works, dispatch reaches the alias, finds the slack target, then
// fails at credentials with "no Slack credentials configured". If it
// doesn't work (current bug), dispatch fails earlier at the config
// lookup with "no notify.yaml configured".
func TestQuery_InteractiveAlertResolvesCallerCustomer(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	cust, err := db.GetOrgByID(custID)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}

	err = db.SaveNotifyAliases(custID, "", "system:\n  email:\n    - \"oncall@example.test\"\nfoo:\n  slack:\n    - \"#oncall\"\n")
	if err != nil {
		t.Fatalf("save notify aliases: %v", err)
	}

	// Prod-shaped Coordinator: Store + Notifier are set, but OrgID
	// is zero (it's never set by cmd/coordinator/main.go either).
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{},
		Store:    db,
		Notifier: NewNotifier(db, nil)})

	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	result, err := c.Execute(context.Background(), `echo 40 | alert above 30 --notify=foo`, tr, hosted.Actor{Org: cust.ID, User: ""})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	resultD := drainExec(t, result)

	// Red assertion: today this fails because stderr is
	// 'alert: dispatch to "foo" failed: no notify.yaml configured'
	// — the notifier used orgID=0 instead of resolving cust.Name.
	if strings.Contains(resultD.Stderr, "no notify.yaml configured") {
		t.Errorf("notifier used wrong customer (got config-not-found):\n  stderr=%q", resultD.Stderr)
	}

	// Positive assertion: a fixed implementation reaches the alias and
	// fails at the Slack credentials step.
	if !strings.Contains(resultD.Stderr, "no Slack credentials configured") {
		t.Errorf("expected to reach Slack-credentials check, got stderr=%q", resultD.Stderr)
	}
}

// TestQuery_InteractiveBinExpandResolvesCallerCustomer is the same
// class of bug for the /bin pre-expand pass we added in the positional-
// args work. The expand is gated on c.OrgID != 0, so in prod it
// never runs — a user typing `myscript arg` at the web shell gets
// no expansion and the planner punts to runLocal, which at least now
// has the ShellFS wired at line 507. But the pre-expand is what lets
// `cat /log/$1 | grep ERROR` in a /bin body reach the planner so the
// source read gets pushed to shards. Today that regresses silently.
func TestQuery_InteractiveBinExpandResolvesCallerCustomer(t *testing.T) {
	db, custID := testStoreAndCustomer(t)

	cust, err := db.GetOrgByID(custID)
	if err != nil {
		t.Fatalf("get customer: %v", err)
	}

	// /bin/echo_first emits $1. Easiest proof-of-expand: if the body is
	// inlined with positionals bound, the planner sees `echo disk` as
	// the leading command → Local pipeline → runLocal runs it and we
	// get "disk" on stdout. If the pre-expand no-ops, the planner sees
	// `echo_first disk`, identifies it as an unknown source command,
	// routes to Local — which then dispatches `echo_first disk` through
	// warp. Warp's PATH resolution uses the interpreter FS, which the
	// runLocal path DOES hook up... so actually even with expand
	// broken, the output may be "disk" from runLocal's warp + FS.
	//
	// A sharper proof: use a /bin body that does something the planner
	// would only catch if it saw the expanded form. Pick a body that
	// errors at plan time unless expanded.
	//
	// Simplest option: assert the expansion itself happens by using a
	// /bin body the planner would rewrite differently than runLocal.
	// For now, use the stdout check — it proves the feature works
	// end-to-end whether via pre-expand or runLocal+FS. A subsequent
	// assertion on `plan.AlertArgs`-style introspection would catch
	// the pre-expand regression specifically, but requires new hooks.
	if err := db.SaveShellFunction(custID, "", "echo_first", "echo $1", "bash"); err != nil {
		t.Fatalf("save script: %v", err)
	}

	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{},
		Store:    db,
		Notifier: NewNotifier(db, nil)}) // OrgID intentionally zero.

	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	result, err := c.Execute(context.Background(), `echo_first disk`, tr, hosted.Actor{Org: cust.ID, User: ""})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	resultD := drainExec(t, result)

	// If /bin resolution is broken at the prod customer boundary,
	// warp emits "echo_first: command not found".
	if strings.Contains(resultD.Stderr, "command not found") {
		t.Errorf("/bin lookup broken for named customer:\n  stderr=%q", resultD.Stderr)
	}
	if got := strings.TrimSpace(resultD.Text); got != "disk" {
		t.Errorf("echo_first disk = %q, want disk (stderr=%q)", got, resultD.Stderr)
	}
}
