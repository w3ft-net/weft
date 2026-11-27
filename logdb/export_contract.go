package logdb

import (
	"errors"
	"time"
)

// The archive export's resources, as the shard FSM and the sweeper see them: a
// snapshot of a shard's volume and a short-lived instance that exports it.
// Implementations live outside the core (ext/launcher/*).

// SnapshotSpec describes the snapshot of a shard's volume for one export attempt.
type SnapshotSpec struct {
	ShardID  string
	Attempt  int
	VolumeID string
}

// ExportSnapshotState is a snapshot's progress as EC2 reports it.
type ExportSnapshotState struct {
	State    string // pending | completed | error | recoverable
	Progress string // e.g. "42%"
}

// ExportSpec describes the instance for one export attempt.
type ExportSpec struct {
	ShardID      string
	Attempt      int
	SnapshotID   string
	InstanceType string
	AZ           string // must have a configured subnet
	Prefix       string // archive prefix the export writes under
}

// ExportResource is one export snapshot or instance found in AWS, with the
// identity its tags carry. Created is zero when EC2 did not report a time.
type ExportResource struct {
	Kind    string // "instance" or "snapshot"
	ID      string
	ShardID string
	Attempt int
	Created time.Time
}

// ErrSnapshotNotFound is wrapped by SnapshotState when the platform does not know
// the snapshot at all, as opposed to it not being ready.
var ErrSnapshotNotFound = errors.New("export snapshot not found")
