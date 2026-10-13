package coordinator

import (
	"context"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/compute"
)

// A glob in a source command that isn't the first command of the line
// (`cd /log/... && tail *.log`, `;` chains, /bin script bodies) must
// expand just like one in the first command. Preflight only sees the
// line's first command, so the expansion has to happen where each query
// pipeline is planned. Unexpanded, `*` reached the planner as the
// literal kind '*' and the query silently returned nothing.
func TestGlobInChainedSource_NoMatchIsAnError(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	const customer = "weftfuncs-test"
	if _, err := db.FindOrCreateNamespace(custID, "acme"); err != nil {
		t.Fatalf("FindOrCreateNamespace: %v", err)
	}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	c.SeenServices.Add(int64(custID), "acme/api/main", "prod")

	for _, line := range []string{
		`cd /log/acme/prod/api && tail -n 1 *.nomatch`,
		`cd /log/acme/prod/api; tail -n 1 *.nomatch`,
		`tail -n 1 /log/acme/prod/api/*.nomatch`,
	} {
		res, err := c.ExecuteWithDir(context.Background(), line, TimeRange{}, customer, "", nil)
		if err == nil {
			d := drainExec(t, res)
			if !strings.Contains(d.Stderr, "no pseudofiles match") {
				t.Errorf("%q: unmatched glob was silent\nstdout: %q\nstderr: %q", line, d.Text, d.Stderr)
			}
			continue
		}
		if !strings.Contains(err.Error(), "no pseudofiles match") {
			t.Errorf("%q: wrong error: %v", line, err)
		}
	}
}

// `cat *.log` (and any glob that would span services or namespaces) is
// refused with an explicit error wherever it sits in the line.
func TestGlobInChainedSource_CrossSourceIsDisallowed(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	const customer = "weftfuncs-test"
	if _, err := db.FindOrCreateNamespace(custID, "acme"); err != nil {
		t.Fatalf("FindOrCreateNamespace: %v", err)
	}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	c.SeenServices.Add(int64(custID), "acme/api/main", "prod")

	for _, line := range []string{
		`cat *.log | grep x`,
		`cd /log/acme && cat *.log | grep x`,
		`cd /log/acme/prod; tail -n 1 *.log`,
		`cat /log/acme/prod/*.log`,
	} {
		res, err := c.ExecuteWithDir(context.Background(), line, TimeRange{}, customer, "", nil)
		got := ""
		if err != nil {
			got = err.Error()
		} else {
			got = drainExec(t, res).Stderr
		}
		if !strings.Contains(got, "not allowed") || !strings.Contains(got, "across sources") {
			t.Errorf("%q: want an explicit cross-source refusal, got %q", line, got)
		}
	}
}

// `tail -f ... > /tmp/x` is refused on the real execution path. The
// rewriter lifts redirects off the query pipeline before it is planned,
// so the planner never sees them; the check has to live where they are
// lifted. Without it the follow ran as a finite read and left a
// truncated file behind, with exit 0.
func TestFollowWithStdoutRedirect_IsRefused(t *testing.T) {
	db, custID := testStoreAndCustomer(t)
	const customer = "weftfuncs-test"
	if _, err := db.FindOrCreateNamespace(custID, "acme"); err != nil {
		t.Fatalf("FindOrCreateNamespace: %v", err)
	}
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})

	for _, line := range []string{
		`tail -f /log/acme/prod/api/main.log > /tmp/stream`,
		`tail -f /log/acme/prod/api/main.log | grep ERROR >> /tmp/stream`,
	} {
		res, err := c.ExecuteWithDir(context.Background(), line, TimeRange{}, customer, "", nil)
		got := ""
		if err != nil {
			got = err.Error()
		} else {
			d := drainExec(t, res)
			got = d.Stderr
		}
		if !strings.Contains(got, "tail -f") || !strings.Contains(got, "can't be redirected") {
			t.Errorf("%q: want a refusal naming tail -f, got %q", line, got)
		}
	}
}
