package logdb

import (
	"context"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"time"
)

// The archive sweeper is the backstop for the ephemeral archive export
// (docs/plans/ephemeral-archive-export.md), the first piece of the account-wide
// orphan reconciliation the platform lacks. It does two jobs on one slow loop:
//
//  1. Remove export snapshots and instances nothing references, so a crash at a
//     bad moment, a failed cleanup, or an admin delete cannot leak them.
//  2. Log an ALERT line for each shard stuck in ARCHIVING. This one runs whether
//     or not the export is enabled: a shard that cannot archive in process is
//     stuck in exactly the same way.
//
// The alert is a log line, not a built-in notification: the cron or monitor that
// pages on it belongs to the operating account's configuration, not to this
// repo.

const (
	// exportOrphanGrace is how long an unreferenced export resource is left
	// alone. The FSM adopts a resource it created but had not yet recorded
	// (the AWS calls are idempotent by tag) within a tick or two, so anything
	// still unreferenced after this is not coming back.
	exportOrphanGrace = 15 * time.Minute

	// exportMaxResourceAge is the absolute cap: an export resource this old is
	// removed even if a shard row still references it. An attempt is failed by
	// the FSM at exportAttemptTimeout, so this only catches what that missed.
	exportMaxResourceAge = exportAttemptTimeout + time.Hour

	// archivingStuckAfter is how long a shard may sit in ARCHIVING before it is
	// alerted on: twice the attempt deadline, so a single failed attempt and its
	// retry do not page.
	archivingStuckAfter = 2 * exportAttemptTimeout

	defaultArchiveSweepInterval = 15 * time.Minute
)

// ExportLister is the sweep's view of AWS. *ExportResources implements it.
type ExportLister interface {
	ListExportResources(ctx context.Context) ([]ExportResource, error)
	DeleteExportResource(ctx context.Context, r ExportResource) error
}

var _ ExportLister = (*ExportResources)(nil)

// archiveSweepStore is the slice of PGShardStore the sweeper reads.
type archiveSweepStore interface {
	GetShard(id string) (*ShardRow, bool, error)
	StuckArchiving(before time.Time) ([]StuckShard, error)
}

// ArchiveSweeper removes orphaned export resources and alerts on stuck
// archives. Resources may be nil (export not enabled): only the alert runs.
type ArchiveSweeper struct {
	Resources ExportLister
	Store     archiveSweepStore
	Clock     func() time.Time
	// IsLeader gates the work to the shard-fsm-leader. Deletes are idempotent,
	// so a second coordinator would be harmless, but it would double every AWS
	// listing for nothing.
	IsLeader func() bool
	Interval time.Duration
}

// exportOrphanReason says why r is an orphan, or "" if it should be kept. sh is
// the shard row r belongs to, nil if there is none. A resource with no known
// creation time is never swept: without an age there is no way to tell an
// orphan from one just created.
func exportOrphanReason(r ExportResource, sh *Shard, now time.Time) string {
	if r.Created.IsZero() {
		return ""
	}
	age := now.Sub(r.Created)
	if age > exportMaxResourceAge {
		return fmt.Sprintf("older than %s", exportMaxResourceAge)
	}
	if age <= exportOrphanGrace {
		return ""
	}
	switch {
	case sh == nil:
		return "its shard row is gone"
	case sh.State == StateArchived || sh.State == StateGone:
		return fmt.Sprintf("its shard is %s", sh.State)
	case r.ID != sh.Export.SnapshotID && r.ID != sh.Export.Instance:
		return "not referenced by the shard's current attempt"
	}
	return ""
}

// SweepExportOnce removes every orphaned export resource and returns how many
// it removed. A failed delete is reported but does not stop the rest.
func (s *ArchiveSweeper) SweepExportOnce(ctx context.Context) (int, error) {
	if s.Resources == nil {
		return 0, nil
	}
	resources, err := s.Resources.ListExportResources(ctx)
	if err != nil {
		return 0, err
	}
	now := s.Clock()
	var (
		deleted int
		errs    []error
		shards  = map[string]*Shard{}
	)
	for _, r := range resources {
		sh, seen := shards[r.ShardID]
		if !seen {
			row, found, err := s.Store.GetShard(r.ShardID)
			if err != nil {
				errs = append(errs, fmt.Errorf("look up shard %s: %w", r.ShardID, err))
				continue
			}
			if found {
				sh = entryOf(row)
			}
			shards[r.ShardID] = sh
		}
		reason := exportOrphanReason(r, sh, now)
		if reason == "" {
			continue
		}
		if err := s.Resources.DeleteExportResource(ctx, r); err != nil {
			errs = append(errs, err)
			continue
		}
		deleted++
		weftsdk.Infof(ctx, "archive-sweep: removed orphaned export %s %s (shard %s attempt %d): %s", r.Kind, r.ID, r.ShardID, r.Attempt, reason)
	}
	return deleted, errors.Join(errs...)
}

// RunOnce is one pass: alert on stuck archives, then sweep orphans.
func (s *ArchiveSweeper) RunOnce(ctx context.Context) error {
	if s.IsLeader != nil && !s.IsLeader() {
		return nil
	}
	var errs []error
	stuck, err := s.Store.StuckArchiving(s.Clock().Add(-archivingStuckAfter))
	if err != nil {
		errs = append(errs, fmt.Errorf("find stuck archives: %w", err))
	}
	for _, sh := range stuck {
		weftsdk.Infof(ctx, "ALERT archiving-stuck shard=%s age=%s export_attempts=%d: still ARCHIVING; see the shardsm and archive lines for this shard",
			sh.ShardID, s.Clock().Sub(sh.Since).Round(time.Minute), sh.Attempts)
	}
	if _, err := s.SweepExportOnce(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Start runs RunOnce on an interval until ctx ends.
func (s *ArchiveSweeper) Start(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = defaultArchiveSweepInterval
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := s.RunOnce(ctx); err != nil {
					weftsdk.Infof(ctx, "archive-sweep: %v", err)
				}
			}
		}
	}()
}
