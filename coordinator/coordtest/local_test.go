package coordtest_test

import (
	"testing"

	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/coordinator/coordtest"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/logtable"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

// localSubject is the open build's stores: one SQLite database, with the one
// group the default shape has. Under a shape with group columns the open build
// has no group to run queries as, so those suites skip.
func localSubject(t *testing.T) coordtest.Subject {
	t.Helper()
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := defaults.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return coordtest.Subject{
		Crons: st.Crons, Sources: st.Sources, Bins: st.Bins, Notify: st.Notify, Claims: st.Coord, Shards: st.Shards,
		Owner:     shape.Added{},
		FactTable: func(ns, svc, kind string) string { return logtable.Name(nil, ns, svc, kind) },
		NewCoordinator: func(t *testing.T, cfg coordinator.CoordinatorConfig) *coordinator.Coordinator {
			if coordinator.NoGroup(shape.Added{}) {
				t.Skip("the open build has no group under a shape with required columns")
			}
			cfg.Crons, cfg.Bins, cfg.Sources, cfg.Notices = st.Crons, st.Bins, st.Sources, st.Notices
			cfg.Mounts = &mounts.Backends{
				Sources:    st.Sources,
				BinService: binscript.Service{Bins: st.Bins, Crons: st.Crons, Sources: st.Sources},
				Notify:     st.Notify,
				CronAuthor: cronauthor.Service{Crons: st.Crons, Sources: st.Sources, Bins: st.Bins},
				Tmp:        st.Tmp,
			}
			cfg.Shards = st.Shards
			cfg.Orgs, cfg.Scoper, cfg.Authz = defaults.OpenOrgs{}, defaults.OpenScoper{}, defaults.OpenAuthorizer{}
			return coordinator.NewCoordinator(cfg)
		},
		PrincipalOf: func(user string) identity.Principal { return defaults.OpenPrincipal{User: user} },
	}
}

func TestAlerts_Local(t *testing.T)             { coordtest.Alerts(t, localSubject) }
func TestAlertQueries_Local(t *testing.T)       { coordtest.AlertQueries(t, localSubject) }
func TestCronRuns_Local(t *testing.T)           { coordtest.CronRuns(t, localSubject) }
func TestQueries_Local(t *testing.T)            { coordtest.Queries(t, localSubject) }
func TestCronQueries_Local(t *testing.T)        { coordtest.CronQueries(t, localSubject) }
func TestRangeQueries_Local(t *testing.T)       { coordtest.RangeQueries(t, localSubject) }
func TestKeyedAggregates_Local(t *testing.T)    { coordtest.KeyedAggregates(t, localSubject) }
func TestTableQueries_Local(t *testing.T)       { coordtest.TableQueries(t, localSubject) }
func TestProjections_Local(t *testing.T)        { coordtest.Projections(t, localSubject) }
func TestStreaming_Local(t *testing.T)          { coordtest.Streaming(t, localSubject) }
func TestTraceQueries_Local(t *testing.T)       { coordtest.TraceQueries(t, localSubject) }
func TestShellFS_Local(t *testing.T)            { coordtest.ShellFS(t, localSubject) }
func TestFollowQueries_Local(t *testing.T)      { coordtest.FollowQueries(t, localSubject) }
func TestScriptTimeouts_Local(t *testing.T)     { coordtest.ScriptTimeouts(t, localSubject) }
func TestBinQueries_Local(t *testing.T)         { coordtest.BinQueries(t, localSubject) }
func TestBinDownstream_Local(t *testing.T)      { coordtest.BinDownstream(t, localSubject) }
func TestFailover_Local(t *testing.T)           { coordtest.Failover(t, localSubject) }
func TestFailoverConcurrent_Local(t *testing.T) { coordtest.FailoverConcurrent(t, localSubject) }
func TestBasics_Local(t *testing.T)             { coordtest.Basics(t, localSubject) }
func TestNamespaceQueries_Local(t *testing.T)   { coordtest.NamespaceQueries(t, localSubject) }
func TestGRPCQueries_Local(t *testing.T)        { coordtest.GRPCQueries(t, localSubject) }
