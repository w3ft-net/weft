// Package coordtest is the behaviour suites of the coordinator that both builds
// must show: the open one over SQLite stores and the hosted one over Postgres.
// A suite names the owner only through the shape's added columns and reaches its
// stores through their contracts, so a build's own test supplies a Subject and
// calls the suite, as cronstoretest does for the stores.
package coordtest

import (
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/coordination"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/logdb"
	"github.com/w3ft-net/weft/notifycfg"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/tenancy"
)

// Subject is the stores a suite runs against, fresh and empty, and the owner
// whose data they hold. The open build's owner is the empty value.
//
// The suites that run queries need the rest: Bins and Notify are the owner's
// scripts and notification config, NewCoordinator builds a coordinator over the
// stores as the build wires one, and ActorOf is the owner acting as a user.
type Subject struct {
	Crons   cron.Store
	Sources source.Store
	Bins    bin.Store
	Notify  notifycfg.Service
	Owner   shape.Added
	// Claims is where a cron executor takes its run slots.
	Claims coordination.Claims

	// Shards is where the coordinator's shard state lives. Stamp sets the tenant
	// columns on a row of the owner, and StampOther on a row of another tenant
	// (nil for a build with one).
	Shards     logdb.ShardStore
	Stamp      func(*pb.LogRow)
	StampOther func(*pb.LogRow)

	// FactTable names the logical table of the owner's source ns/svc/kind, as a
	// shard's field facts are keyed.
	FactTable func(ns, svc, kind string) string

	NewCoordinator func(t *testing.T, cfg coordinator.CoordinatorConfig) *coordinator.Coordinator
	ActorOf        func(user string) tenancy.Actor
}

// saveWatch saves an enabled cron named name, homed in namespace ns, and returns it.
func (s Subject) saveWatch(t *testing.T, ns, name string) cron.Watch {
	t.Helper()
	home, err := s.Sources.FindOrCreateNamespace(s.Owner, ns)
	if err != nil {
		t.Fatal(err)
	}
	spec := cron.WatchSpec{
		Schedule: "every 1m", Pipeline: "true", Enabled: true,
		Compiled: cron.Compiled{ValueType: cron.ValueNumber},
	}
	if err := s.Crons.SaveWatch(s.Owner, home.ID, "", name, spec); err != nil {
		t.Fatal(err)
	}
	w, err := s.Crons.GetWatch(s.Owner, home.ID, name)
	if err != nil {
		t.Fatal(err)
	}
	return *w
}

// record saves one numeric result per value for w, a minute apart and ending a
// minute ago, under tag.
func (s Subject) record(t *testing.T, w cron.Watch, tag string, values ...float64) {
	t.Helper()
	now := time.Now()
	for i, v := range values {
		v := v
		r := cron.Result{
			WatchID: w.ID, RanAt: now.Add(-time.Duration(len(values)-i) * time.Minute),
			Stdout: tag + " " + formatValue(v), Value: &v, Tag: tag,
		}
		if err := s.Crons.SaveResults([]cron.Result{r}); err != nil {
			t.Fatal(err)
		}
	}
}
