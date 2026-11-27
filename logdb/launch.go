package logdb

import "context"

// ShardVolumeDevice is the block device a shard's data volume is attached as; the
// host's startup mounts it from this name (/dev/sdf).
const ShardVolumeDevice = "/dev/sdf"

// What the shard FSM needs of a platform that launches shard instances.
// Implementations live outside the core (ext/launcher/*).

// DefaultShardVolumeGB is the fallback size for a newly-created shard data
// volume when the tier has no SHARD_<TIER>_VOLUME_GB override wired through
// (dev mode, or a misconfigured prod env). Production sizes come per-tier from
// Terraform via ProvisionerConfig.VolumeSizesGB — see logdb.parseShardVolumeSizes.
// Sized to the small tier's realistic floor so a missing override never
// over-provisions: on a t4g.micro the binding disk constraint is co-located
// DuckDB spill headroom, not data, and ~25 GiB holds a ~12 GiB .duckdb plus an
// equal spill arena (see duck-db-tuning.md).
const DefaultShardVolumeGB int32 = 25

// ShardLaunchAPI is the launch primitive the shard FSM depends on. Narrow
// so test fakes don't have to stand up a whole EC2 client.
type ShardLaunchAPI interface {
	Launch(ctx context.Context, spec LaunchSpec) (LaunchResult, error)
	Terminate(ctx context.Context, instanceID string) error
}

// LaunchSpec describes one shard instance to bring up. ExistingVolumeID
// distinguishes a fresh launch (empty: create a new gp3 volume) from a
// recovery launch (set: reattach the surviving volume). The (Tier, AZ)
// pair must resolve to a configured subnet + the tier to a launch template,
// or Launch errors fast — silently falling back would hide config drift.
type LaunchSpec struct {
	ShardID          string
	Tier             string
	AZ               string
	ExistingVolumeID string // "" → fresh (create volume); set → recovery (reattach)
	SizeGB           int32  // fresh-launch volume size; ignored on recovery
}

// LaunchResult carries the identifiers the shard SM persists onto its entry
// once a launch completes.
type LaunchResult struct {
	InstanceID string
	VolumeID   string
	AZ         string
}

// VolumeInfo is a flattened view of an EBS volume relevant to shard
// lifecycle decisions.
type VolumeInfo struct {
	VolumeID string
	AZ       string
	State    string // available / in-use / creating / deleting / ...
	ShardID  string // from the shard_id tag; empty for volumes created pre-christening
	Tier     string // from the shard_tier tag
	// InstanceID is the instance the volume is attached (or attaching) to,
	// "" when it is available. An upgrade's swap and rollback re-derive
	// their progress from it.
	InstanceID string
}

// CreateVolumeOpts parameterises a CreateForShard call.
type CreateVolumeOpts struct {
	ShardID string
	Tier    string // small / medium / large; stored as shard_tier tag
	AZ      string
	SizeGB  int32
}

// VolumeManager wraps the EC2 volume APIs with a narrow
// interface the coordinator can use without dragging the full SDK
// into every call site. Pure wrappers — decisions about which call
// to make (christen vs recover, archive vs retain) live in the
// provisioner, not here.
type VolumeManager interface {
	CreateForShard(ctx context.Context, opts CreateVolumeOpts) (volumeID string, err error)
	FindByShard(ctx context.Context, shardID string) (*VolumeInfo, error)
	ListAvailableByTier(ctx context.Context, tier, az string) ([]VolumeInfo, error)
	TagWithShardID(ctx context.Context, volumeID, shardID string) error
	AttachToInstance(ctx context.Context, volumeID, instanceID, device string) error
	DetachFromInstance(ctx context.Context, volumeID string, force bool) error
	DeleteVolume(ctx context.Context, volumeID string) error
}
