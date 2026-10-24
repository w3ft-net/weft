package fleetaudit

import (
	"fmt"
	"sort"
)

// The fleet audit (docs/plans/shard-seamless-upgrade-testing.md) is the check
// run after every live upgrade/chaos test: at rest, the database's picture of
// the fleet and EC2's must agree, and nothing may be mid-move. AuditFleet is
// pure; `weft-admin audit` gathers the two views and prints its violations.

// AuditShard is the shard_records slice the audit needs.
type AuditShard struct {
	ID           string
	State        string // logdb.ShardState
	InstanceID   string
	VolumeID     string
	UpgradePhase string // logdb.UpgradePhase
}

// AuditSpare is one shard_spares row.
type AuditSpare struct {
	InstanceID string
	Claimed    bool
}

// AuditInstance is a weft shard-role EC2 instance. State is EC2's; ShardID
// is the shard_id tag, Spare the shard_spare tag, DataVolumes the attached
// non-root volumes.
type AuditInstance struct {
	ID          string
	State       string
	ShardID     string
	Spare       bool
	DataVolumes []string
}

// AuditVolume is a weft shard EBS volume. AttachedTo is empty unless attached.
type AuditVolume struct {
	ID         string
	State      string
	ShardID    string
	AttachedTo string
}

// FleetView is both halves of the fleet at one moment.
type FleetView struct {
	Shards    []AuditShard
	Spares    []AuditSpare
	Instances []AuditInstance
	Volumes   []AuditVolume
}

// Shard states by name; this package stays free of logdb (and so of DuckDB)
// so weft-admin remains a CGO-free binary.
const (
	stateIngesting = "INGESTING"
	stateReadOnly  = "READ_ONLY"
	stateUpgrading = "UPGRADING"
	stateDegraded  = "DEGRADED"
	stateSuspended = "SUSPENDED"
	stateLaunching = "LAUNCHING"
	stateArchived  = "ARCHIVED"
	stateGone      = "GONE"
)

func instanceLive(state string) bool { return state == "pending" || state == "running" }

// AuditFleet returns one line per violated invariant; none means healthy.
func AuditFleet(v FleetView) []string {
	var out []string
	add := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	shards := make(map[string]AuditShard, len(v.Shards))
	for _, sh := range v.Shards {
		shards[sh.ID] = sh
	}
	instances := make(map[string]AuditInstance, len(v.Instances))
	liveByShard := map[string][]AuditInstance{}
	for _, in := range v.Instances {
		instances[in.ID] = in
		if instanceLive(in.State) && in.ShardID != "" {
			liveByShard[in.ShardID] = append(liveByShard[in.ShardID], in)
		}
	}
	volumes := make(map[string]AuditVolume, len(v.Volumes))
	for _, vol := range v.Volumes {
		volumes[vol.ID] = vol
	}

	for _, sh := range v.Shards {
		switch sh.State {
		case stateUpgrading:
			add("%s: UPGRADING in phase %s", sh.ID, sh.UpgradePhase)
			continue
		case stateIngesting, stateReadOnly:
		case stateDegraded, stateSuspended, stateLaunching:
			add("%s: state %s, not serving", sh.ID, sh.State)
			continue
		default:
			continue // ARCHIVING/ARCHIVED/GONE hold no serving instance
		}
		if sh.UpgradePhase != "" {
			add("%s: upgrade_phase %s left set on a %s shard", sh.ID, sh.UpgradePhase, sh.State)
		}
		live := liveByShard[sh.ID]
		switch {
		case len(live) == 0:
			add("%s: no running instance tagged shard_id", sh.ID)
			continue
		case len(live) > 1:
			add("%s: %d running instances tagged shard_id", sh.ID, len(live))
			continue
		}
		if sh.InstanceID != live[0].ID {
			add("%s: row names instance %s, EC2 runs %s", sh.ID, sh.InstanceID, live[0].ID)
			continue
		}
		if vol, ok := volumes[sh.VolumeID]; !ok || vol.AttachedTo != sh.InstanceID {
			add("%s: volume %s is not attached to %s", sh.ID, sh.VolumeID, sh.InstanceID)
		}
	}

	rows := map[string]AuditSpare{}
	for _, sp := range v.Spares {
		rows[sp.InstanceID] = sp
		if in, ok := instances[sp.InstanceID]; !ok || !instanceLive(in.State) || !in.Spare {
			add("spare %s: row has no running spare instance", sp.InstanceID)
		}
		if sp.Claimed {
			add("spare %s: still claimed", sp.InstanceID)
		}
	}
	for _, in := range v.Instances {
		if !instanceLive(in.State) || !in.Spare {
			continue
		}
		if _, ok := rows[in.ID]; !ok {
			add("spare %s: running spare instance has no shard_spares row", in.ID)
		}
		if len(in.DataVolumes) > 0 {
			add("spare %s: has data volumes %v", in.ID, in.DataVolumes)
		}
	}

	for _, in := range v.Instances {
		if instanceLive(in.State) && in.ShardID != "" {
			if _, ok := shards[in.ShardID]; !ok {
				add("instance %s: tagged shard_id=%s, no such record", in.ID, in.ShardID)
			}
		}
	}
	for _, vol := range v.Volumes {
		if vol.State == "deleting" || vol.State == "deleted" || vol.ShardID == "" {
			continue
		}
		sh, ok := shards[vol.ShardID]
		switch {
		case !ok:
			add("volume %s: tagged shard_id=%s, no such record", vol.ID, vol.ShardID)
		case sh.State == stateArchived || sh.State == stateGone:
			add("volume %s: shard %s is %s", vol.ID, sh.ID, sh.State)
		}
	}

	sort.Strings(out)
	return out
}
