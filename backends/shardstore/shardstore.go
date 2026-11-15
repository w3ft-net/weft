// Package shardstore is the contract of the shard-state domain: the rows a
// shard store persists, the states they move through, and the Store interface
// every build implements (Postgres for the hosted build, SQLite for the open
// one; see defaults and shardstore/shardstoretest).
//
// It imports nothing but the standard library, so a process that links an
// implementation stays cgo-free. The live registry entry, the FSMs and the
// provisioner live in logdb, which embeds these rows.
package shardstore

import (
	"errors"
	"time"
)

// ShardState is the persisted lifecycle state of a shard, written by
// the shard FSM's external-storage mutator (a narrow state-column
// UPDATE) and read back through the stateless ExternalStorage
// accessor.
//
// The states divide cleanly: LAUNCHING/INGESTING/READ_ONLY are the
// "healthy lifecycle" line; ARCHIVING is READ_ONLY's tail — the
// export-in-progress phase, query-routable just like READ_ONLY (it
// keeps serving from the live DuckDB until the export swaps in a
// ColdShard); SUSPENDED is the recoverable-failure branch; ARCHIVED
// is terminal-for-data (data in S3, instance + volume gone) but still
// query-serving via a ColdShard; GONE is the cleanup sink for launch
// failures, data-already-lost cases, and admin deletes.
//
// ARCHIVING is conceptually a substate of READ_ONLY but is NOT a
// stateless SubstateOf: it must not inherit READ_ONLY's
// HeartbeatStale → SUSPENDED edge (its instance is being torn down by
// design). The "substate" relationship lives at the query-routing
// layer instead — MatchingEntries treats ARCHIVING exactly like
// READ_ONLY.
//
// UPGRADING is an operator-driven roll. With the warm pool wired (prod) it
// is a serving state: the shard's volume moves to a claimed standby behind a
// shard-enforced pause, through the persisted UpgradePhase below, and
// UpgradeComplete returns it to the state it came from
// (docs/plans/shard-seamless-upgrade.md). Without the pool (dev) it is the
// operator-driven sibling of SUSPENDED: same reap-then-relaunch recovery, but
// no heartbeat-promotion edge, so the only exit is TriggerRelaunch (post-reap)
// → LAUNCHING or TriggerStranded → GONE. Nothing in the open build
// transitions into it.
//
// See docs/plans/asg-as-api-stateless.md for the full transition table.
type ShardState string

const (
	StateLaunching ShardState = "LAUNCHING"
	StateIngesting ShardState = "INGESTING"
	StateReadOnly  ShardState = "READ_ONLY"
	StateArchiving ShardState = "ARCHIVING"
	StateSuspended ShardState = "SUSPENDED"
	StateUpgrading ShardState = "UPGRADING"
	StateArchived  ShardState = "ARCHIVED"
	StateGone      ShardState = "GONE"
	// StateDegraded is an alive shard whose ingest is persistently failing
	// (consecutive failures crossed SuspendFailureThreshold). It's a
	// circuit-breaker pause on INGESTING: excluded from write routing
	// (ingestCandidate requires INGESTING) but still read-queryable (its
	// data is intact on a live instance) and still counted as part of the
	// bucket's live cohort so the breaker window doesn't churn a
	// replacement. Evaluate re-admits it to INGESTING after a cooldown
	// (half-open); a re-failure degrades it again, and a dead instance
	// drops it to SUSPENDED like any INGESTING shard.
	StateDegraded ShardState = "DEGRADED"
)

// UpgradePhase is where an UPGRADING shard is in the move of its volume to
// a warm-pool standby (docs/plans/shard-seamless-upgrade.md). UPGRADING is a
// serving state: routing never leaves the shard, and calls that arrive
// while its volume is moving are held by the shard's gate and the client.
type UpgradePhase string

const (
	// PhaseClaiming: a standby is being claimed (or launched on demand) and
	// awaited; the old instance serves normally.
	PhaseClaiming UpgradePhase = "claiming"
	// PhasePausing: the old instance refuses new calls with a redirect to
	// the standby and drains, then closes DuckDB and unmounts.
	PhasePausing UpgradePhase = "pausing"
	// PhaseSwapping: the volume moves to the standby, which mounts it and
	// opens DuckDB.
	PhaseSwapping UpgradePhase = "swapping"
	// PhaseResumed: the standby serves the shard. The demoted instance keeps
	// redirecting until every coordinator routes to the new address and the
	// pool's replacement standby heartbeats, then it is terminated.
	PhaseResumed UpgradePhase = "resumed"
	// PhaseRollingBack: the standby never readied; the volume goes back to
	// the demoted instance, which remounts and resumes.
	PhaseRollingBack UpgradePhase = "rolling_back"
)

// VolumeMoving reports whether the shard's volume is between instances in
// this phase: no instance is heartbeating as the shard, and calls are held.
func (p UpgradePhase) VolumeMoving() bool {
	return p == PhasePausing || p == PhaseSwapping || p == PhaseRollingBack
}

// BucketState is the persisted state of a bucket, mirroring ShardState in
// shape. Buckets are coarser-grained than shards: their state communicates
// whether a cascade is in flight and which phase.
//
// There is no ARCHIVING bucket state: archival is a shard concern (the
// shard's ARCHIVING state) that the bucket only fires via a fire-and-forget
// retention sweep. The bucket cascade is purely the rotation that must never
// gap the hot window.
type BucketState string

const (
	BucketStasis      BucketState = "STASIS"
	BucketWarming     BucketState = "WARMING"
	BucketRotating    BucketState = "ROTATING"
	BucketScalingDown BucketState = "SCALING_DOWN"
)

// ShardRow is a persisted shard. The first group of fields is what every
// build keeps. Export, Upgrade and LastPurgedAt are hosted-only facts (the
// ephemeral export and the seamless upgrade need EC2 and warm standbys; the
// retention janitor stamps the purge): zero in the open build, which stores
// none of them.
type ShardRow struct {
	ID              string
	WindowStart     time.Time
	WindowEnd       time.Time
	State           ShardState
	IngestFailures  int    // consecutive ingest failures; resets on success
	Addr            string // gRPC address for live shards
	InstanceID      string // EC2 instance ID
	ParquetPath     string // Parquet file path for archived shards
	Tier            string // instance tier (small/medium/large)
	RowCount        int64
	CreatedAt       time.Time
	EBSVolumeID     string    // EBS volume id for this shard's DuckDB; empty until volume is attached
	HeartbeatToken  string    // per-shard bearer the Heartbeat handler validates against
	BucketID        int64     // surrogate FK to buckets.id
	LastHeartbeatAt time.Time // wall-clock of the most recent observed heartbeat; zero = never (treated as fresh)
	AZ              string    // FSM striping: pins recovery launches to the volume's AZ
	StripeIndex     *int      // bucket-assigned stripe position; nil = unassigned (distinct from 0)
	LaunchAttempts  int       // per-shard launch retry counter
	BinarySHA       string    // shard's compile-time GitSHA from its heartbeat; weft-admin skips a shard whose SHA already matches the target binary
	// DiskBytes is the last non-zero footprint (.duckdb + WAL + spill) the
	// shard reported; it sizes the archive export host.
	DiskBytes int64

	Export  ExportFacts
	Upgrade UpgradeFacts
}

// Clone returns r with its pointers and nested facts copied, so a copy never
// aliases a live entry's.
func (r ShardRow) Clone() ShardRow {
	if r.StripeIndex != nil {
		v := *r.StripeIndex
		r.StripeIndex = &v
	}
	r.Export = r.Export.Clone()
	r.Upgrade = r.Upgrade.Clone()
	return r
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// ExportFacts are the persisted facts of an ephemeral archive export.
// Attempt is monotonic and survives clearing the rest.
type ExportFacts struct {
	SnapshotID   string
	Instance     string
	InstanceType string
	StartedAt    *time.Time
	Attempt      int
}

// Clone returns f with its time pointer copied.
func (f ExportFacts) Clone() ExportFacts {
	f.StartedAt = copyTime(f.StartedAt)
	return f
}

// UpgradeFacts are the persisted facts of an upgrade in flight. The zero
// value clears them.
type UpgradeFacts struct {
	Phase               UpgradePhase
	Origin              ShardState
	PhaseAt             *time.Time
	PausedAt            *time.Time // first Pause sent: when callers start holding
	SuccessorInstanceID string
	SuccessorAddr       string
	DemotedInstanceID   string
	// DemotedAddr is cleared when the demoted instance was terminated
	// rather than paused (a pre-upgrade binary): there is no instance to
	// resume, redirect from, or terminate at the end.
	DemotedAddr string
	// TraceID is the trace of the RelaunchShard call that started the
	// upgrade. A tick has no request of its own, so the phase code logs
	// under this id.
	TraceID string
	// ParentSpanID is the span of that call, the parent of the spans the
	// phase code records.
	ParentSpanID string
}

// Clone returns f with its time pointers copied.
func (f UpgradeFacts) Clone() UpgradeFacts {
	f.PhaseAt = copyTime(f.PhaseAt)
	f.PausedAt = copyTime(f.PausedAt)
	return f
}

// ExportClaim is the outcome of BeginExportAttempt.
type ExportClaim int

const (
	// ExportClaimWon: this caller started the shard's next attempt.
	ExportClaimWon ExportClaim = iota
	// ExportClaimLost: another evaluator already claimed this shard's next
	// attempt (a leader handover), or the shard row is gone.
	ExportClaimLost
	// ExportClaimHeld: the cap on concurrent attempts is reached. Nothing was
	// written: no attempt number was taken and no clock started.
	ExportClaimHeld
)

// StuckShard is a shard that has been ARCHIVING since Since.
type StuckShard struct {
	ShardID  string
	Since    time.Time
	Attempts int
}

// Bucket is a group of shards that rotate together. Buckets don't serve
// services: shards do (ShardService). The service→bucket assignment lives in
// the sources registry.
type Bucket struct {
	ID                   int64
	Label                string
	TargetIngestingCount int
	Tier                 string
	CreatedAt            string
	WindowDuration       string
	// State is the bucket FSM state; empty reads as STASIS.
	State string
	// CascadeStartedAt is set when a cascade begins and cleared when it ends;
	// nil when none is in flight.
	CascadeStartedAt *time.Time
}

// ShardService records that a shard serves (NamespaceID, Service), the
// stamp future queries route through.
type ShardService struct {
	ID          int64
	ShardID     string
	NamespaceID uint
	Service     string
}

// ShardFieldFact is one typed field fact a shard reported.
type ShardFieldFact struct {
	ShardID    string
	CustomerID int64
	Namespace  string
	Service    string
	Kind       string
	Env        string
	Locator    string
	Epoch      int64
}

// ShardHeartbeat is one beat from a shard (or, with an empty ShardID, from an
// unclaimed instance).
type ShardHeartbeat struct {
	ShardID    string
	Addr       string
	InstanceID string
	State      string
	RowCount   int64
	DiskBytes  int64
	BinarySHA  string // shard binary's compile-time GitSHA; weft-admin reads it for skip-if-current. Not persisted to the heartbeat table.
	// Age is how long before it was read this beat was recorded, by the
	// store's own clock. Zero for a beat just received over RPC. Carried as
	// an age rather than a timestamp so replaying it needs no agreement
	// between the coordinator's clock and the store's.
	Age time.Duration
	// Standby marks a warm-pool instance's beat (it has no volume and no shard
	// id). Standby beats refresh the pool's row and are never recorded here or
	// dispatched as a shard's.
	Standby bool
}

var (
	// ErrShardMoved is SetShardAddr's refusal: the row names a different
	// instance than the one the address belongs to, so the shard has moved on.
	ErrShardMoved = errors.New("shard row names a different instance")
	// ErrUnknownBucket is returned by BucketState when the bucket row is gone.
	ErrUnknownBucket = errors.New("bucket record not found")
	// ErrUpgradeMoved is returned by SetShardUpgrade when the row is no longer
	// in the phase the writer last saw: another coordinator advanced the
	// upgrade, and a writer working from a stale view must not overwrite it.
	ErrUpgradeMoved = errors.New("upgrade moved on")
)

// Store is the persistence contract of the shard-state domain: shard rows,
// buckets, the service membership shards serve, field facts, heartbeats and the
// facts of an archive export and an upgrade in flight. Every build implements
// it.
//
// Narrow writes touch only the columns they name, so a lifecycle writer cannot
// trample another's fields. The store records every change of a shard's state
// (including its creation and deletion) in a history that LatestPreRecoveryState
// and StuckArchiving read, however the change was made.
type Store interface {
	ExportStore
	UpgradeStore

	// SaveShard upserts a shard row.
	SaveShard(sh *ShardRow) error
	// GetShard returns (row, true, nil) when found, (nil, false, nil) when
	// absent.
	GetShard(id string) (*ShardRow, bool, error)
	ListShards() ([]*ShardRow, error)
	// ListShardsByBucket returns a bucket's shard rows, newest window first,
	// archived shards included.
	ListShardsByBucket(bucketID int64) ([]*ShardRow, error)
	// DeleteShard removes a shard row; a missing one is not an error.
	DeleteShard(id string) error

	SetShardState(id string, state ShardState) error
	SetShardAddr(id, instanceID, addr string) error
	SetShardIngestFailures(id string, n int) error
	SetShardLaunchResult(id, instanceID, az, volumeID string, stripeIndex *int) error
	IncrementShardLaunchAttempts(id string) error
	SetShardParquetPath(id, path string) error
	ClearShardVolume(id string) error
	ClearShardInstance(id string) error
	// TouchHeartbeat writes the heartbeat-owned fields. diskBytes is written
	// only when positive: a beat that reports no footprint must not erase the
	// last known one.
	TouchHeartbeat(id string, rowCount int64, at time.Time, binarySHA string, diskBytes int64) error
	// LatestPreRecoveryState returns the state the shard was in immediately
	// before its most recent transition into UPGRADING or SUSPENDED, "" if it
	// has none.
	LatestPreRecoveryState(id string) (ShardState, error)
	// StuckArchiving returns the ARCHIVING shards whose latest entry into
	// ARCHIVING is before the given time.
	StuckArchiving(before time.Time) ([]StuckShard, error)

	// StampShardServices records services as the ones the shard serves;
	// idempotent.
	StampShardServices(shardID string, services []ShardService) error
	AddShardService(shardID string, namespaceID uint, service string) error
	ListShardServices() ([]ShardService, error)
	// ShardIDsServingService returns the shards serving (namespaceID, service);
	// an empty service narrows to the namespace only.
	ShardIDsServingService(namespaceID uint, service string) (map[string]struct{}, error)

	ListShardFieldFacts() ([]ShardFieldFact, error)
	// ApplyShardFieldFacts upserts typed and deletes dropped, for shardID.
	ApplyShardFieldFacts(shardID string, typed, dropped []ShardFieldFact) error

	// RecordHeartbeat upserts the latest beat of a source.
	RecordHeartbeat(hb *ShardHeartbeat) error
	// LiveShardHeartbeats returns the beats within the given duration: the
	// newest per claimed shard, and every unclaimed instance's own.
	LiveShardHeartbeats(within time.Duration) ([]ShardHeartbeat, error)
	PruneShardHeartbeats(maxAge time.Duration) error

	ListBuckets() ([]Bucket, error)
	GetBucket(id int64) (Bucket, bool, error)
	GetBucketByLabel(label string) (Bucket, bool, error)
	// SaveBucket inserts a bucket with no ID, assigning one back, and
	// otherwise updates it in place.
	SaveBucket(b *Bucket) error
	BucketState(id int64) (BucketState, error)
	// SetBucketState writes only the state column.
	SetBucketState(id int64, st BucketState) error
	// SetBucketCascadeStart records when a cascade began; nil clears it.
	SetBucketCascadeStart(id int64, at *time.Time) error
	ShardsInBucketStates(bucketID int64, states ...ShardState) ([]*ShardRow, error)
}

// ExportStore persists the ephemeral archive export's facts. It is part of
// Store: SaveShard writes the same facts, and every read of a shard row returns
// them.
type ExportStore interface {
	// BeginExportAttempt claims the next export attempt: it moves the attempt
	// counter from prevAttempt to prevAttempt+1 provided it is still
	// prevAttempt (else ExportClaimLost) and fewer than maxActive other
	// shards have an attempt in flight (else ExportClaimHeld).
	BeginExportAttempt(id string, prevAttempt int, instanceType string, startedAt time.Time, maxActive int) (ExportClaim, error)
	SetExportSnapshot(id, snapshotID string) error
	SetExportInstance(id, instanceID string) error
	// ClearExportFacts ends the attempt's bookkeeping but keeps the counter.
	ClearExportFacts(id string) error
	LatestShardDiskBytes(id string) (int64, error)
}

// UpgradeStore persists the seamless upgrade's facts. It is part of Store, like
// ExportStore.
type UpgradeStore interface {
	// SetShardUpgrade is a compare-and-set of a shard's upgrade facts on the
	// phase the writer last saw (from; "" means none). The zero UpgradeFacts
	// clears them. A stale writer gets ErrUpgradeMoved.
	SetShardUpgrade(id string, from UpgradePhase, f UpgradeFacts) error
}
