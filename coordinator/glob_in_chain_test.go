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
	c := NewCoordinator(CoordinatorConfig{Pool: &compute.LocalComputePool{}, Store: db})
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
	c := NewCoordinator(CoordinatorConfig{Pool: &compute.LocalComputePool{}, Store: db})
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
