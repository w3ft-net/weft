package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/backends/notice"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

// A batch the drain cannot place is recorded as an ingestion failure, so the api
// can tell the tenant which of its services are affected. The recorder used to
// be constructed by nobody, so every failure the drain reported was dropped and
// /api/ingestion-status always answered that nothing was wrong
// (docs/known-issues.md, "IngestionErrorRecorder is never constructed").
//
// The coordinator the process runs is built from the adapter, so that is where
// the test starts: the failure must reach the store the adapter supplies.
func TestWithTenancy_AnUnplaceableBatchIsRecordedAsAnIngestionFailure(t *testing.T) {
	if coordinator.NoTenant(shape.Added{}) {
		t.Skip("the open build has no tenant under a shape with required columns")
	}
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := defaults.Open(db)
	if err != nil {
		t.Fatal(err)
	}

	a := withContracts(Adapter[*testDB]{Identity: noIdentity})
	a.Crons = func(*testDB) cron.Store { return st.Crons }
	a.Bins = func(*testDB) bin.Store { return st.Bins }
	a.Sources = func(*testDB) source.Store { return st.Sources }
	a.Notices = func(*testDB) notice.Store { return st.Notices }
	a.Shards = func(*testDB) logdb.ShardStore { return st.Shards }
	a.IngestFailures = func(*testDB) ingestfail.Store { return st.Ingest }

	cfg := a.WithTenancy(&testDB{}, coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Ingest: coordinator.NewRingBuffer(1000)})
	c := coordinator.NewCoordinator(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	// No shard serves ops/api, so the drain has nowhere to put the batch.
	c.Ingest.Write(coordinator.RowsOf(shape.Added{}, []*pb.LogRow{
		{Namespace: "ops", Service: "api", Kind: "main", Raw: "a line", TsUnixNs: time.Now().UnixNano()},
	}))

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := st.Ingest.AffectedServices(shape.Added{}, time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if slices.Equal(got, []string{"ops/api"}) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("affected services = %v, want [ops/api]: the drain's failure was not recorded", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
