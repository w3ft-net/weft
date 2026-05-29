package main

import (
	"context"
	"fmt"
	"time"
)

// shardStateGetter abstracts "ask the coordinator what state this shard is in
// right now, when it last heartbeated, and what binary SHA it reported."
// Production wires it to a pb.WeftAdminClient.GetShard call; tests fake it.
type shardStateGetter func(shardID string) (state string, lastHeartbeatAt time.Time, binarySHA string, err error)

// pollShardReady waits for a shard to land in INGESTING with a heartbeat
// stamped AFTER `minLastHB` (the relaunch cutoff) and — when `wantSHA` is
// non-empty — with binary_sha == wantSHA. Returns nil on success or an
// error (getter error / ctx deadline).
//
// The cutoff matters because the row briefly reads INGESTING mid-transition
// with the PRE-relaunch heartbeat — if the relaunch fires right after a
// heartbeat, the pre-relaunch last_heartbeat_at is already "now-ish" and
// would pass a "freshness-of-now" check.
//
// The SHA gate matters because a relaunched shard that comes up with the OLD
// binary (e.g. its instance failed to fetch the new AMI, or stripped past
// the launch template version) would otherwise be confirmed as successfully
// rolled — silently masking a broken deploy. With wantSHA set the poll only
// succeeds once the new shard reports the SHA we asked for.
func pollShardReady(ctx context.Context, get shardStateGetter, shardID string, minLastHB time.Time, wantSHA string, interval time.Duration) error {
	for {
		state, hb, sha, err := get(shardID)
		if err != nil {
			return fmt.Errorf("poll %s: %w", shardID, err)
		}
		if state == "INGESTING" && hb.After(minLastHB) && (wantSHA == "" || sha == wantSHA) {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for %s to return to INGESTING (last state=%s, last_heartbeat_at=%v, binary_sha=%q, cutoff=%v, want_sha=%q): %w", shardID, state, hb, sha, minLastHB, wantSHA, ctx.Err())
		case <-time.After(interval):
		}
	}
}
