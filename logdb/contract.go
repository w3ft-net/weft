package logdb

import "github.com/w3ft-net/weft/backends/shardstore"

// The shard-state contract lives in shardstore (cgo-free, so the open build's
// store can use it); logdb aliases it so the FSMs, the registry and the
// provisioner keep speaking in these names. See shardstore for the docs.
type (
	ShardState   = shardstore.ShardState
	UpgradePhase = shardstore.UpgradePhase
	BucketState  = shardstore.BucketState
	ShardRow     = shardstore.ShardRow
	ExportFacts  = shardstore.ExportFacts
	UpgradeFacts = shardstore.UpgradeFacts
	ExportClaim  = shardstore.ExportClaim
	StuckShard   = shardstore.StuckShard
	ShardStore   = shardstore.Store
	ExportStore  = shardstore.ExportStore
	UpgradeStore = shardstore.UpgradeStore
)

const (
	StateLaunching = shardstore.StateLaunching
	StateIngesting = shardstore.StateIngesting
	StateReadOnly  = shardstore.StateReadOnly
	StateArchiving = shardstore.StateArchiving
	StateSuspended = shardstore.StateSuspended
	StateUpgrading = shardstore.StateUpgrading
	StateArchived  = shardstore.StateArchived
	StateGone      = shardstore.StateGone
	StateDegraded  = shardstore.StateDegraded

	PhaseClaiming    = shardstore.PhaseClaiming
	PhasePausing     = shardstore.PhasePausing
	PhaseSwapping    = shardstore.PhaseSwapping
	PhaseResumed     = shardstore.PhaseResumed
	PhaseRollingBack = shardstore.PhaseRollingBack

	BucketStasis      = shardstore.BucketStasis
	BucketWarming     = shardstore.BucketWarming
	BucketRotating    = shardstore.BucketRotating
	BucketScalingDown = shardstore.BucketScalingDown

	ExportClaimWon  = shardstore.ExportClaimWon
	ExportClaimLost = shardstore.ExportClaimLost
	ExportClaimHeld = shardstore.ExportClaimHeld
)

var (
	ErrShardMoved    = shardstore.ErrShardMoved
	ErrUnknownBucket = shardstore.ErrUnknownBucket
	ErrUpgradeMoved  = shardstore.ErrUpgradeMoved
)
