package fleetaudit

import (
	"strings"
	"testing"
)

// healthyFleet is two serving shards (one per AZ), each on its own running
// instance with its volume attached, plus one volume-less spare per AZ.
func healthyFleet() FleetView {
	return FleetView{
		Shards: []AuditShard{
			{ID: "s-a", State: stateIngesting, InstanceID: "i-a", VolumeID: "vol-a"},
			{ID: "s-b", State: stateReadOnly, InstanceID: "i-b", VolumeID: "vol-b"},
		},
		Spares: []AuditSpare{{InstanceID: "i-sp1"}, {InstanceID: "i-sp2"}},
		Instances: []AuditInstance{
			{ID: "i-a", State: "running", ShardID: "s-a", DataVolumes: []string{"vol-a"}},
			{ID: "i-b", State: "running", ShardID: "s-b", DataVolumes: []string{"vol-b"}},
			{ID: "i-sp1", State: "running", Spare: true},
			{ID: "i-sp2", State: "running", Spare: true},
		},
		Volumes: []AuditVolume{
			{ID: "vol-a", State: "in-use", ShardID: "s-a", AttachedTo: "i-a"},
			{ID: "vol-b", State: "in-use", ShardID: "s-b", AttachedTo: "i-b"},
		},
	}
}

func TestAuditFleet(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(v *FleetView)
		want   string // substring of exactly one violation; "" = healthy
	}{
		{"healthy", func(v *FleetView) {}, ""},
		{"terminated instances are ignored", func(v *FleetView) {
			v.Instances = append(v.Instances, AuditInstance{ID: "i-dead", State: "terminated", ShardID: "s-a"})
		}, ""},
		{"deleting volume is ignored", func(v *FleetView) {
			v.Volumes = append(v.Volumes, AuditVolume{ID: "vol-x", State: "deleting", ShardID: "ghost"})
		}, ""},
		{"shard instance missing", func(v *FleetView) {
			v.Instances = v.Instances[1:]
		}, "s-a: no running instance"},
		{"two instances claim the shard", func(v *FleetView) {
			v.Instances = append(v.Instances, AuditInstance{ID: "i-twin", State: "running", ShardID: "s-a"})
		}, "s-a: 2 running instances"},
		{"shard row names a different instance", func(v *FleetView) {
			v.Shards[0].InstanceID = "i-other"
		}, "s-a: row names instance i-other"},
		{"volume detached", func(v *FleetView) {
			v.Volumes[0] = AuditVolume{ID: "vol-a", State: "available", ShardID: "s-a"}
		}, "s-a: volume vol-a is not attached to i-a"},
		{"volume on the wrong instance", func(v *FleetView) {
			v.Volumes[0].AttachedTo = "i-b"
		}, "s-a: volume vol-a is not attached to i-a"},
		{"spare row without instance", func(v *FleetView) {
			v.Instances = v.Instances[:3]
		}, "spare i-sp2: row has no running spare instance"},
		{"spare instance without row", func(v *FleetView) {
			v.Spares = v.Spares[:1]
		}, "spare i-sp2: running spare instance has no shard_spares row"},
		{"claimed spare at rest", func(v *FleetView) {
			v.Spares[0].Claimed = true
		}, "spare i-sp1: still claimed"},
		{"spare holds a data volume", func(v *FleetView) {
			v.Instances[2].DataVolumes = []string{"vol-a"}
		}, "spare i-sp1: has data volumes"},
		{"instance tagged with unknown shard", func(v *FleetView) {
			v.Instances = append(v.Instances, AuditInstance{ID: "i-orphan", State: "running", ShardID: "ghost"})
		}, "instance i-orphan: tagged shard_id=ghost, no such record"},
		{"volume tagged with unknown shard", func(v *FleetView) {
			v.Volumes = append(v.Volumes, AuditVolume{ID: "vol-orphan", State: "available", ShardID: "ghost"})
		}, "volume vol-orphan: tagged shard_id=ghost, no such record"},
		{"shard stuck upgrading", func(v *FleetView) {
			v.Shards[0].State = stateUpgrading
			v.Shards[0].UpgradePhase = "swapping"
		}, "s-a: UPGRADING in phase swapping"},
		{"upgrade phase left on a serving shard", func(v *FleetView) {
			v.Shards[0].UpgradePhase = "resumed"
		}, "s-a: upgrade_phase resumed left set"},
		{"degraded shard", func(v *FleetView) {
			v.Shards[0].State = stateDegraded
		}, "s-a: state DEGRADED"},
		{"archived shard still has a volume", func(v *FleetView) {
			v.Shards = append(v.Shards, AuditShard{ID: "s-old", State: stateArchived})
			v.Volumes = append(v.Volumes, AuditVolume{ID: "vol-old", State: "available", ShardID: "s-old"})
		}, "volume vol-old: shard s-old is ARCHIVED"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := healthyFleet()
			tc.mutate(&v)
			got := AuditFleet(v)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want healthy, got %q", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("want exactly one violation containing %q, got %q", tc.want, got)
			}
		})
	}
}
