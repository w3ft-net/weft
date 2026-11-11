package logdb

import (
	"context"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"math"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// The ephemeral archive export, from the shard FSM's side
// (docs/plans/ephemeral-archive-export.md). ARCHIVING stays one state; the
// export's phase is derived from the persisted export_* facts on the shard row,
// and each Evaluate tick advances it as far as it can without waiting on AWS:
//
//	no attempt in flight   → claim the next attempt (and size the instance)
//	no snapshot            → snapshot the shard's volume
//	snapshot pending       → wait
//	no export instance     → launch it (unless the concurrency cap holds it)
//	instance running       → wait for the manifest
//	manifest, this attempt → record the archive, then clean up
//	instance ended, no manifest / deadline passed / snapshot errored
//	                       → fail the attempt: clean up, clear the facts
//
// The shard's own volume and instance are retired only after the archive is
// recorded and the export resources are gone (evaluateArchiving).

const (
	// exportAttemptTimeout bounds one attempt from its claim, snapshot wait and
	// all. It must exceed the export binary's own --timeout (150m, see
	// deploy/weft-export.service) so the instance gives up first and a hung host
	// is the only thing this catches.
	exportAttemptTimeout = 3 * time.Hour

	// maxExportAttempts caps retries. Past it the shard stays ARCHIVING with its
	// volume and an alert-worthy log line each tick; it is never auto-GONE.
	// Resetting export_attempt to 0 re-arms it.
	maxExportAttempts = 3

	// defaultMaxConcurrentExports caps export instances in flight across all
	// shards, to stay under EC2 vCPU quotas when a bucket cohort reaches
	// ARCHIVING together.
	defaultMaxConcurrentExports = 2
)

// ExportOps is the AWS surface the export phases use. *ExportResources is the
// production implementation; tests fake it.
type ExportOps interface {
	CreateSnapshot(ctx context.Context, spec SnapshotSpec) (string, error)
	SnapshotState(ctx context.Context, snapshotID string) (ExportSnapshotState, error)
	LaunchInstance(ctx context.Context, spec ExportSpec) (string, error)
	InstanceState(ctx context.Context, instanceID string) (string, error)
	Cleanup(ctx context.Context, shardID string) error
}

var _ ExportOps = (*ExportResources)(nil)

// ExportSize maps a shard's on-disk size to the export instance type for it:
// the smallest class whose MaxDBBytes is at least the size.
type ExportSize struct {
	MaxDBBytes   int64
	InstanceType string
}

// defaultExportSizing is the seed table. SEED VALUES: the coefficients behind
// them (memory per archive partition, throughput per instance type) are to be
// measured from the Phase 0 archive logs, not known yet. Override per
// deployment through ShardDeps.ExportSizing once they are.
var defaultExportSizing = []ExportSize{
	{MaxDBBytes: 8 << 30, InstanceType: "r7g.large"},
	{MaxDBBytes: 24 << 30, InstanceType: "r7g.xlarge"},
	{MaxDBBytes: math.MaxInt64, InstanceType: "r7g.2xlarge"},
}

// exportInstanceType picks the export instance for a shard of dbBytes on the
// given attempt: the smallest class that fits, one class larger for each retry
// (a failed derived size is treated as a formula miss, not retried blind), and
// never past the largest. An unknown size (0: no heartbeat to read) takes the
// largest class rather than guessing small.
func (d *ShardDeps) exportInstanceType(dbBytes int64, attempt int) (string, error) {
	table := d.ExportSizing
	if len(table) == 0 {
		table = defaultExportSizing
	}
	idx := len(table) - 1
	if dbBytes > 0 {
		for i, s := range table {
			if dbBytes <= s.MaxDBBytes {
				idx = i
				break
			}
		}
	}
	if attempt > 1 {
		idx += attempt - 1
	}
	if idx > len(table)-1 {
		idx = len(table) - 1
	}
	if table[idx].InstanceType == "" {
		return "", fmt.Errorf("export sizing table has no instance type at index %d", idx)
	}
	return table[idx].InstanceType, nil
}

func (d *ShardDeps) maxConcurrentExports() int {
	if d.MaxConcurrentExports > 0 {
		return d.MaxConcurrentExports
	}
	return defaultMaxConcurrentExports
}

// exportInstanceEnded reports whether an EC2 instance state means the export
// host is done or going away. "" (EC2 doesn't know the instance) is
// deliberately not ended: EC2 can briefly not know an instance it just
// launched, and a host that really vanished is caught by exportAttemptTimeout.
func exportInstanceEnded(state string) bool {
	switch state {
	case "shutting-down", "terminated", "stopping", "stopped":
		return true
	}
	return false
}

// exportS3Prefix is the archive prefix the export uses. It is the in-process
// archiveS3Prefix with the service label sanitized, because the prefix does not
// only name an S3 location: it travels through an EC2 tag, the export host's
// shell, and a systemd environment file, and a shard's sole service name is
// customer-controlled (the ingest API only requires it to be non-empty). A name
// with a space, quote or newline would otherwise be rejected by the host's
// validation and fail every attempt. Coordinator and host must agree on the
// prefix, so launch, manifest polling and output cleanup all use this one.
func (sh *Shard) exportS3Prefix(services []store.ShardService) string {
	return fmt.Sprintf("%s/%s", sh.deps.ParquetDir, exportLabel(serviceNames(services)))
}

// maxExportLabelLen keeps the prefix well inside EC2's 256-character tag value.
const maxExportLabelLen = 64

// exportLabel is joinServices with every character outside [A-Za-z0-9._-]
// replaced by '_', truncated, and never "." or "..".
func exportLabel(apps []string) string {
	label := joinServices(apps)
	var b strings.Builder
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if len(s) > maxExportLabelLen {
		s = s[:maxExportLabelLen]
	}
	if s == "" || strings.Trim(s, ".") == "" {
		return "_"
	}
	return s
}

// hasExportFacts reports whether any export resource is recorded on the shard.
func (sh *Shard) hasExportFacts() bool {
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	return sh.ExportSnapshotID != "" || sh.ExportInstanceID != ""
}

// advanceExport moves the export as far forward as it can without waiting on
// AWS. It returns with sh.ParquetPath set when the archive has been recorded.
// AWS errors are logged and left for the next tick; only persistence errors are
// returned.
func (sh *Shard) advanceExport(ctx context.Context) error {
	d := sh.deps
	sh.mu.RLock()
	attempt, snapID, instID := sh.ExportAttempt, sh.ExportSnapshotID, sh.ExportInstanceID
	started, itype := sh.ExportStartedAt, sh.ExportInstanceType
	volumeID, az, services := sh.EBSVolumeID, sh.AZ, sh.Services
	sh.mu.RUnlock()
	now := d.Clock()

	// No attempt in flight: claim the next one.
	if started == nil {
		if attempt >= maxExportAttempts {
			weftsdk.Errorf(ctx, "shardsm %s: ARCHIVING export failed %d times (cap %d); keeping volume %q and instance — reset export_attempt to retry",
				sh.ID, attempt, maxExportAttempts, volumeID)
			return nil
		}
		// A shard with no volume has nothing to snapshot. Claiming a slot for it
		// would hold one of the few export slots for the whole attempt deadline,
		// for every attempt, blocking healthy shards; it waits unclaimed instead
		// (the stuck-archive alert surfaces it).
		if volumeID == "" {
			weftsdk.Infof(ctx, "shardsm %s: ARCHIVING export has no volume to snapshot; not claiming an export slot", sh.ID)
			return nil
		}
		diskBytes, err := d.Store.LatestShardDiskBytes(sh.ID)
		if err != nil {
			return fmt.Errorf("shardsm %s: read disk size for export sizing: %w", sh.ID, err)
		}
		typ, err := d.exportInstanceType(diskBytes, attempt+1)
		if err != nil {
			return fmt.Errorf("shardsm %s: %w", sh.ID, err)
		}
		claim, err := d.Store.BeginExportAttempt(sh.ID, attempt, typ, now, d.maxConcurrentExports())
		if err != nil {
			return fmt.Errorf("shardsm %s: begin export attempt: %w", sh.ID, err)
		}
		switch claim {
		case ExportClaimHeld:
			// Nothing started: no attempt number taken, no clock running, no AWS
			// call made. The shard simply waits for a slot, however long that takes.
			weftsdk.Infof(ctx, "shardsm %s: export held, the cap of %d concurrent export attempts is reached", sh.ID, d.maxConcurrentExports())
			return nil
		case ExportClaimLost:
			weftsdk.Infof(ctx, "shardsm %s: another evaluator claimed export attempt %d; deferring to it", sh.ID, attempt+1)
			return nil
		}
		attempt++
		itype = typ
		started = &now
		sh.mu.Lock()
		sh.ExportAttempt, sh.ExportInstanceType, sh.ExportStartedAt = attempt, typ, started
		sh.mu.Unlock()
		weftsdk.Infof(ctx, "shardsm %s: export attempt %d/%d begun (db≈%d bytes → %s)", sh.ID, attempt, maxExportAttempts, diskBytes, typ)
	}

	if now.Sub(*started) > exportAttemptTimeout {
		return sh.failExportAttempt(ctx, fmt.Sprintf("attempt exceeded %s", exportAttemptTimeout))
	}

	// Snapshot the shard's volume.
	if snapID == "" {
		if volumeID == "" {
			weftsdk.Infof(ctx, "shardsm %s: ARCHIVING export has no volume to snapshot", sh.ID)
			return nil
		}
		id, err := d.Exports.CreateSnapshot(ctx, SnapshotSpec{ShardID: sh.ID, Attempt: attempt, VolumeID: volumeID})
		if err != nil {
			weftsdk.Infof(ctx, "shardsm %s: export snapshot of %s, retrying next tick: %v", sh.ID, volumeID, err)
			return nil
		}
		if err := d.Store.SetExportSnapshot(sh.ID, id); err != nil {
			return fmt.Errorf("shardsm %s: persist export snapshot: %w", sh.ID, err)
		}
		sh.mu.Lock()
		sh.ExportSnapshotID = id
		sh.mu.Unlock()
		snapID = id
	}
	snap, err := d.Exports.SnapshotState(ctx, snapID)
	if errors.Is(err, errSnapshotNotFound) {
		// Also what a half-finished failure looks like: cleanup removed the
		// snapshot but a later step did not complete. Failing again re-runs the
		// idempotent cleanup instead of polling a snapshot that will never return.
		return sh.failExportAttempt(ctx, fmt.Sprintf("snapshot %s no longer exists", snapID))
	}
	if err != nil {
		weftsdk.Infof(ctx, "shardsm %s: export snapshot %s state, retrying next tick: %v", sh.ID, snapID, err)
		return nil
	}
	switch snap.State {
	case "completed":
	case "error":
		return sh.failExportAttempt(ctx, fmt.Sprintf("snapshot %s entered the error state", snapID))
	default:
		return nil // pending: poll again next tick
	}

	// Launch the export instance. The concurrency cap was already applied when
	// this attempt was claimed, so there is nothing to check here.
	if instID == "" {
		id, err := d.Exports.LaunchInstance(ctx, ExportSpec{
			ShardID: sh.ID, Attempt: attempt, SnapshotID: snapID, InstanceType: itype, AZ: az,
			Prefix: sh.exportS3Prefix(services),
		})
		if err != nil {
			weftsdk.Infof(ctx, "shardsm %s: export instance launch, retrying next tick: %v", sh.ID, err)
			return nil
		}
		if err := d.Store.SetExportInstance(sh.ID, id); err != nil {
			return fmt.Errorf("shardsm %s: persist export instance: %w", sh.ID, err)
		}
		sh.mu.Lock()
		sh.ExportInstanceID = id
		sh.mu.Unlock()
		weftsdk.Infof(ctx, "shardsm %s: export attempt %d launched %s (%s) from %s", sh.ID, attempt, id, itype, snapID)
		return nil
	}

	// Wait for the manifest.
	key := shard.ManifestKey(sh.exportS3Prefix(services), sh.ID)
	m, found, err := d.ExportManifests.Get(ctx, key)
	if err != nil {
		weftsdk.Infof(ctx, "shardsm %s: poll export manifest %s, retrying next tick: %v", sh.ID, key, err)
		return nil
	}
	if found && m.Attempt == attempt {
		return sh.recordArchive(m.ParquetPath, m.Partitions)
	}
	state, err := d.Exports.InstanceState(ctx, instID)
	if err != nil {
		weftsdk.Infof(ctx, "shardsm %s: export instance %s state, retrying next tick: %v", sh.ID, instID, err)
		return nil
	}
	if !exportInstanceEnded(state) {
		return nil
	}
	// The host writes its manifest and then powers off, so it can end between
	// our two polls. Look once more before calling it a failure.
	m, found, err = d.ExportManifests.Get(ctx, key)
	if err != nil {
		weftsdk.Infof(ctx, "shardsm %s: re-poll export manifest %s, retrying next tick: %v", sh.ID, key, err)
		return nil
	}
	if found && m.Attempt == attempt {
		return sh.recordArchive(m.ParquetPath, m.Partitions)
	}
	return sh.failExportAttempt(ctx, fmt.Sprintf("export instance %s ended (%s) without a manifest", instID, state))
}

// deleteAttemptOutput removes the current attempt's partial output, reporting
// whether it is done (nothing to delete, or deleted). It never touches anything
// once an archive is recorded, and only ever builds paths of the form
// <id>.attempt-N, so it cannot reach the in-process layout or another attempt.
func (sh *Shard) deleteAttemptOutput() bool {
	sh.mu.RLock()
	attempt, recorded, services := sh.ExportAttempt, sh.ParquetPath, sh.Services
	sh.mu.RUnlock()
	if recorded != "" {
		weftsdk.Infof(context.Background(), "shardsm %s: archive already recorded at %s; not deleting export output", sh.ID, recorded)
		return true
	}
	if sh.deps.ExportOutputs == nil {
		weftsdk.Infof(context.Background(), "shardsm %s: no output deleter configured; attempt %d's partial output (if any) is left in place", sh.ID, attempt)
		return true
	}
	for _, p := range shard.ExportOutputPaths(sh.exportS3Prefix(services), sh.ID, attempt) {
		if err := sh.deps.ExportOutputs.DeleteParquet(p); err != nil {
			weftsdk.Errorf(context.Background(), "shardsm %s: delete failed attempt %d's output %s, retrying next tick: %v", sh.ID, attempt, p, err)
			return false
		}
	}
	return true
}

// failExportAttempt ends the current attempt unsuccessfully: it removes the
// attempt's AWS resources and clears its facts, leaving export_attempt so the
// next tick claims the following one (or stops, at the cap).
func (sh *Shard) failExportAttempt(ctx context.Context, reason string) error {
	sh.mu.RLock()
	attempt := sh.ExportAttempt
	sh.mu.RUnlock()
	weftsdk.Errorf(ctx, "shardsm %s: export attempt %d/%d failed: %s", sh.ID, attempt, maxExportAttempts, reason)
	return sh.endExportAttempt(ctx, true)
}

// endExportAttempt removes the shard's export resources, then clears the facts.
// With discardOutput (a failed attempt) it also deletes the attempt's partial
// Parquet, after the host is stopped so a still-running host cannot write after
// the delete. A successful attempt passes false: its output is the archive.
// A failed step leaves the facts in place so the next tick retries; the error
// is logged, not returned, like the other AWS steps.
func (sh *Shard) endExportAttempt(ctx context.Context, discardOutput bool) error {
	if err := sh.deps.Exports.Cleanup(ctx, sh.ID); err != nil {
		weftsdk.Infof(ctx, "shardsm %s: export cleanup, retrying next tick: %v", sh.ID, err)
		return nil
	}
	if discardOutput && !sh.deleteAttemptOutput() {
		return nil
	}
	if err := sh.deps.Store.ClearExportFacts(sh.ID); err != nil {
		return fmt.Errorf("shardsm %s: clear export facts: %w", sh.ID, err)
	}
	sh.mu.Lock()
	sh.ExportSnapshotID, sh.ExportInstanceID, sh.ExportInstanceType, sh.ExportStartedAt = "", "", "", nil
	sh.mu.Unlock()
	return nil
}
