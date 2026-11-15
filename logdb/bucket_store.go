package logdb

import "time"

// bucketStore is the persistence the bucket FSM needs: its own state column,
// the cascade clock, and the cohort of shard rows it derives its plan from.
type bucketStore interface {
	// BucketState returns the bucket's persisted FSM state, ErrUnknownBucket
	// when the row is gone. A row that predates the state column reads as
	// STASIS so the FSM converges on the next tick.
	BucketState(id int64) (BucketState, error)
	// SetBucketState writes only the state column, so it cannot race a
	// config writer over the lifecycle-owned ones.
	SetBucketState(id int64, st BucketState) error
	// SetBucketCascadeStart records when a cascade began; nil clears it (no
	// cascade in flight).
	SetBucketCascadeStart(id int64, at *time.Time) error
	// ShardsInBucketStates returns the bucket's shard rows in any of states.
	ShardsInBucketStates(bucketID int64, states ...ShardState) ([]*ShardRow, error)
}
