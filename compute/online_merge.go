package compute

import (
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// onlineCursor holds the per-shard FIFO of unemitted rows. A cursor is
// "ready" when len(queue) > 0 OR sentinel is true (its remaining queue
// can be drained without blocking on more shard input). A cursor is
// "exhausted" when len(queue) == 0 AND sentinel is true — the merge
// will skip it on subsequent passes.
type onlineCursor struct {
	queue    []*pb.ResultRow
	sentinel bool
}

// onlineMerge drains ch until shardCount sentinels arrive, calling
// emit for each row in mergeKey order as soon as the global merge can
// determine the next row.
//
// Streaming property: emit fires BEFORE every shard has sent its
// sentinel. For two shards A and B with mergeKey "ts":
//   - A sends row 1 first → no emission yet (B might have a smaller row).
//   - B sends row 2 → emit can fire on A's row 1 (1 < 2, both heads known).
//   - As more rows arrive on either side, emission advances incrementally.
//
// When emit returns false (e.g. limit reached) onlineMerge keeps
// draining ch so the producers don't deadlock, but stops calling emit.
//
// mergeKey accepts the same values as mergeRows ("ts"[ DESC],
// "raw"[ DESC]). Unknown or empty keys fall back to FIFO arrival order
// — fine for the planner's Mode C path where downstream stages decide
// the global order anyway.
func onlineMerge(
	ch <-chan ComputeMessage,
	shardCount int,
	mergeKey string,
	emit func(*pb.ResultRow) bool,
) {
	less := makeLess(mergeKey)
	if less == nil {
		// Concat / arrival order: no merge, emit each row as it arrives.
		sentinels := 0
		emitting := true
		for sentinels < shardCount {
			msg := <-ch
			if msg.Sentinel {
				sentinels++
				continue
			}
			if !emitting || msg.Row == nil {
				continue
			}
			if !emit(msg.Row) {
				emitting = false
			}
		}
		return
	}

	cursors := make(map[string]*onlineCursor)
	sentinels := 0
	emitting := true

	addCursor := func(shardID string) *onlineCursor {
		c, ok := cursors[shardID]
		if !ok {
			c = &onlineCursor{}
			cursors[shardID] = c
		}
		return c
	}

	for sentinels < shardCount {
		msg := <-ch
		if msg.Sentinel {
			sentinels++
			addCursor(msg.ShardID).sentinel = true
		} else if msg.Row != nil {
			cur := addCursor(msg.ShardID)
			cur.queue = append(cur.queue, msg.Row)
		}
		if emitting {
			if !drainReady(cursors, shardCount, less, emit) {
				emitting = false
			}
		}
	}
}

// drainReady emits rows in merge order while every shard is "ready"
// (queue head present OR sentinel'd). Stops when:
//   - some shard has yet to be heard from (len(cursors) < shardCount),
//   - some live (non-sentinel) cursor has an empty queue (waiting on
//     more rows or a sentinel),
//   - all cursors are exhausted, or
//   - emit returns false (caller is done; e.g. limit hit).
//
// Returns true normally; false if emit asked to stop.
func drainReady(
	cursors map[string]*onlineCursor,
	shardCount int,
	less func(a, b *pb.ResultRow) bool,
	emit func(*pb.ResultRow) bool,
) bool {
	for {
		if len(cursors) < shardCount {
			return true
		}
		var minCur *onlineCursor
		for _, c := range cursors {
			if len(c.queue) == 0 {
				if c.sentinel {
					continue
				}
				return true
			}
			if minCur == nil || less(c.queue[0], minCur.queue[0]) {
				minCur = c
			}
		}
		if minCur == nil {
			return true
		}
		row := minCur.queue[0]
		minCur.queue = minCur.queue[1:]
		if !emit(row) {
			return false
		}
	}
}

// makeLess returns a comparator for the given merge key, or nil for
// concat (arrival-order) semantics. Mirrors mergeRowsFull's switch so
// the streaming and batch merges agree on what each key means.
func makeLess(mergeKey string) func(a, b *pb.ResultRow) bool {
	key, desc := parseMergeKey(mergeKey)
	var less func(a, b *pb.ResultRow) bool
	switch key {
	case "ts":
		less = lessTS
	case "raw":
		less = func(a, b *pb.ResultRow) bool { return a.Raw < b.Raw }
	default:
		return nil
	}
	if desc {
		orig := less
		less = func(a, b *pb.ResultRow) bool { return orig(b, a) }
	}
	return less
}

// lessTS orders by (ts, event_id, line_no), a row with no event_id first:
// the order a shard sorts its rows in, so rows tied on ts from two shards
// merge deterministically and an event's lines stay together and in order
// (an event lives on one shard).
func lessTS(a, b *pb.ResultRow) bool {
	if a.TsUnixNs != b.TsUnixNs {
		return a.TsUnixNs < b.TsUnixNs
	}
	if a.EventId != b.EventId {
		return a.EventId < b.EventId
	}
	return a.LineNo < b.LineNo
}
