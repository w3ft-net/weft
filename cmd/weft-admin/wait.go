package main

import (
	"context"
	"fmt"
	"time"
)

// shardStateGetter abstracts "ask the coordinator what state this shard is in
// right now, and when it last heartbeated." Production wires it to a
// pb.WeftCoordinatorClient.GetShard call; tests fake it.
type shardStateGetter func(shardID string) (state string, lastHeartbeatAt time.Time, err error)

// pollShardReady waits for a shard to land in INGESTING with a heartbeat
// stamped AFTER the caller's `minLastHB` cutoff (the moment the relaunch
// was fired), polling every `interval`. Returns nil on success or an error
// (getter error / ctx deadline).
//
// minLastHB matters because the row briefly reads INGESTING mid-transition
// with the PRE-relaunch heartbeat — if the relaunch fires right after a
// heartbeat, the pre-relaunch last_heartbeat_at is already "now-ish" and
// would pass any "freshness-of-now" check. Demanding a post-cutoff
// timestamp is the unambiguous "we're looking at the post-relaunch shard"
// signal.
func pollShardReady(ctx context.Context, get shardStateGetter, shardID string, minLastHB time.Time, interval time.Duration) error {
	for {
		state, hb, err := get(shardID)
		if err != nil {
			return fmt.Errorf("poll %s: %w", shardID, err)
		}
		if state == "INGESTING" && hb.After(minLastHB) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for %s to return to INGESTING (last state=%s, last_heartbeat_at=%v, cutoff=%v): %w", shardID, state, hb, minLastHB, ctx.Err())
		case <-time.After(interval):
		}
	}
}
