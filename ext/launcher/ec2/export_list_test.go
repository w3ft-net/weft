package ec2

import (
	"context"
	"testing"
	"time"

	"github.com/w3ft-net/weft/logdb"
)

// ── ExportResources.ListExportResources / DeleteExportResource ──────

func TestExportResources_ListFindsOnlyExportResourcesWithTheirIdentity(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)
	ctx := context.Background()
	created := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)

	snapID, _ := x.CreateSnapshot(ctx, logdb.SnapshotSpec{ShardID: "s-1", Attempt: 2, VolumeID: "v"})
	api.snaps[0].started = created
	instID, _ := x.LaunchInstance(ctx, logdb.ExportSpec{ShardID: "s-1", Attempt: 2, SnapshotID: snapID, InstanceType: "r7g.large", AZ: "us-east-2a", Prefix: "s3://b/p"})
	api.insts[0].launched = created

	// Things that must not be listed: a shard's own instance and an already
	// terminated export instance.
	api.insts = append(api.insts, &fakeEC2Instance{id: "i-shard", tags: map[string]string{"shard_id": "s-1"}, state: "running"})
	api.insts = append(api.insts, &fakeEC2Instance{id: "i-dead", state: "terminated", launched: created,
		tags: map[string]string{"export_shard_id": "s-9", "export_attempt": "1"}})

	got, err := x.ListExportResources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]logdb.ExportResource{}
	for _, r := range got {
		byID[r.ID] = r
	}
	if len(got) != 2 {
		t.Fatalf("listed %d resources, want the snapshot and the live instance only: %+v", len(got), got)
	}
	if r := byID[snapID]; r.Kind != "snapshot" || r.ShardID != "s-1" || r.Attempt != 2 || !r.Created.Equal(created) {
		t.Errorf("snapshot = %+v", r)
	}
	if r := byID[instID]; r.Kind != "instance" || r.ShardID != "s-1" || r.Attempt != 2 || !r.Created.Equal(created) {
		t.Errorf("instance = %+v", r)
	}
}

func TestExportResources_DeleteExportResource(t *testing.T) {
	api := &fakeExportEC2{}
	x := newTestExportResources(api)
	ctx := context.Background()
	snapID, _ := x.CreateSnapshot(ctx, logdb.SnapshotSpec{ShardID: "s-1", Attempt: 1, VolumeID: "v"})
	instID, _ := x.LaunchInstance(ctx, logdb.ExportSpec{ShardID: "s-1", Attempt: 1, SnapshotID: snapID, InstanceType: "r7g.large", AZ: "us-east-2a", Prefix: "s3://b/p"})

	if err := x.DeleteExportResource(ctx, logdb.ExportResource{Kind: "instance", ID: instID}); err != nil {
		t.Fatal(err)
	}
	if err := x.DeleteExportResource(ctx, logdb.ExportResource{Kind: "snapshot", ID: snapID}); err != nil {
		t.Fatal(err)
	}
	if len(api.terminated) != 1 || api.terminated[0] != instID || len(api.deletedSnapshots) != 1 || api.deletedSnapshots[0] != snapID {
		t.Fatalf("terminated=%v deleted=%v", api.terminated, api.deletedSnapshots)
	}

	// Already gone is success: the sweep races the FSM's own cleanup.
	if err := x.DeleteExportResource(ctx, logdb.ExportResource{Kind: "instance", ID: "i-nope"}); err != nil {
		t.Errorf("deleting an unknown instance: %v", err)
	}
	if err := x.DeleteExportResource(ctx, logdb.ExportResource{Kind: "snapshot", ID: "snap-nope"}); err != nil {
		t.Errorf("deleting an unknown snapshot: %v", err)
	}
	if err := x.DeleteExportResource(ctx, logdb.ExportResource{Kind: "volume", ID: "vol-1"}); err == nil {
		t.Error("deleting an unknown kind succeeded")
	}
}
