package coordinator

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/mem"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// wfEnv is a group's crons and namespaces over the local stores, for the tests
// of the weft:: functions, which read only the cron contract.
type wfEnv struct {
	crons   *mem.Crons
	sources *mem.Sources
	notify  notifycfg.Service
	owner   shape.Added
}

func newWFEnv(t *testing.T) *wfEnv {
	t.Helper()
	sources := mem.NewSources()
	crons := mem.NewCrons(sources)
	nl := mem.NewNotify()
	return &wfEnv{crons: crons, sources: sources, notify: notifycfg.Service{Aliases: nl, Secrets: nl}, owner: shapetest.Added(1)}
}

// wfPrincipal is the system identity of the env's group.
type wfPrincipal struct{ owner shape.Added }

func (a wfPrincipal) Partition() shape.Added { return a.owner }
func (wfPrincipal) Subject() string          { return "" }

// wfScoper lets the principal read all of its own group.
type wfScoper struct{}

func (wfScoper) ReadScope(_ context.Context, a identity.Principal) (shape.Set, error) {
	return shape.Set{a.Partition().Coord()}, nil
}

// wf is a WeftFuncs reading as the system identity, which may read every
// namespace.
func (e *wfEnv) wf() *WeftFuncs {
	return &WeftFuncs{Crons: e.crons, Sources: e.sources, Owner: e.owner, Scoper: wfScoper{},
		Ctx: identity.WithPrincipal(context.Background(), wfPrincipal{e.owner})}
}

// saveWatch saves an enabled cron named name in namespace "ops", where the
// weft:: function tests keep their crons (they name it "ops/<name>"), and
// returns it.
func (e *wfEnv) saveWatch(t *testing.T, name string) cron.Watch {
	t.Helper()
	ops, err := e.sources.FindOrCreateNamespace(e.owner, "ops")
	if err != nil {
		t.Fatal(err)
	}
	spec := cron.WatchSpec{Schedule: "every 1m", Pipeline: "echo test", Enabled: true, Compiled: cron.Compiled{ValueType: cron.ValueNumber}}
	if err := e.crons.SaveWatch(e.owner, ops.ID, "", name, spec); err != nil {
		t.Fatal(err)
	}
	w, err := e.crons.GetWatch(e.owner, ops.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	return *w
}

// saveResult stores r, taking its value from its stdout when it has none and
// stdout reads as a number: what the run path does before it saves a result.
func (e *wfEnv) saveResult(t *testing.T, r cron.Result) {
	t.Helper()
	if r.Value == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(r.Stdout), 64); err == nil {
			r.Value = &v
		}
	}
	if err := e.crons.SaveResults([]cron.Result{r}); err != nil {
		t.Fatal(err)
	}
}

// seedWatch saves the cron name with one result per value, interval apart and
// ending now.
func (e *wfEnv) seedWatch(t *testing.T, name string, values []string, interval time.Duration) {
	t.Helper()
	w := e.saveWatch(t, name)
	now := time.Now()
	for i, v := range values {
		e.saveResult(t, cron.Result{WatchID: w.ID, RanAt: now.Add(-time.Duration(len(values)-1-i) * interval), Stdout: v})
	}
}
