package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// An archived shard's descriptor says which layout its archive was written in,
// so compute can pass it to the cold-scan Lambda. A live shard's does not: its
// own volume records it.
func TestShardDescriptorsFor_ArchivedCarriesTheLayout(t *testing.T) {
	p := NewProvisioner(NewShardRegistry(), nil, ProvisionerConfig{
		PollInterval:      time.Second,
		LambdaFunctionURL: "https://fn.example/",
	})
	now := time.Now()
	cold, err := shard.NewColdShard("cold", nil, "s3://b/cold/sized/**/*.parquet", "https://fn.example/", nil)
	if err != nil {
		t.Fatal(err)
	}
	SeedShard(p, &Shard{
		Live: cold, ID: "cold", State: StateArchived, ParquetPath: "s3://b/cold/sized/**/*.parquet", RowCount: 10,
		WindowStart: now.Add(-6 * time.Hour), WindowEnd: now.Add(-2 * time.Hour), CreatedAt: now.Add(-6 * time.Hour),
	})
	SeedShard(p, &Shard{
		ID: "hot", State: StateIngesting, Addr: "10.0.0.1:9000",
		WindowStart: now.Add(-time.Hour), WindowEnd: now.Add(time.Hour), CreatedAt: now.Add(-time.Hour),
	})

	ds, err := p.ShardDescriptorsFor(context.Background(), TimeRange{Start: now.Add(-72 * time.Hour), End: now}, 0, "", time.Now().Add(-store.LogRetention(store.TierTrial)))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ds {
		switch d.ShardId {
		case "cold":
			if got := shard.LayoutFromProto(d.Layout); got.Fingerprint != shape.BuildLayout().Fingerprint || d.Layout == nil {
				t.Errorf("archived descriptor layout = %+v, want the build's", d.Layout)
			}
		case "hot":
			if d.Layout != nil {
				t.Errorf("live descriptor carries a layout: %+v", d.Layout)
			}
		}
	}
	if len(ds) != 2 {
		var ids []string
		for _, d := range ds {
			ids = append(ids, d.ShardId)
		}
		t.Fatalf("got descriptors %v, want both shards", ids)
	}
}
