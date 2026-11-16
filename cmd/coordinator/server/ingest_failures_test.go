package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/w3ft-net/weft/compute"
	"github.com/w3ft-net/weft/coordinator"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// A batch the drain cannot place is recorded as an ingestion failure, so the api
// can tell the group which of its services are affected. The recorder used to
// be constructed by nobody, so every failure the drain reported was dropped and
// /api/ingestion-status always answered that nothing was wrong.
//
// The coordinator the process runs is built from the deployment, so that is where
// the test starts: the failure must reach the backend the deployment supplies.
func TestCoordinatorConfig_AnUnplaceableBatchIsRecordedAsAnIngestionFailure(t *testing.T) {
	if coordinator.NoGroup(shape.Added{}) {
		t.Skip("the open build has no group under a shape with required columns")
	}
	d, own := testDeployment(t, nil)
	st := d.Backends

	cfg := CoordinatorConfig(d, own, coordinator.CoordinatorConfig{Compute: &compute.Engine{}, Ingest: coordinator.NewRingBuffer(1000)})
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
		got, err := st.IngestFailures.AffectedServices(shape.Added{}, time.Now().Add(-time.Hour))
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
