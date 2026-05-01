package compute

import (
	"container/heap"
	"strings"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// mergeRows k-way merges a pre-sorted set of per-shard row slices into
// a flat slice of row.Raw strings honoring mergeKey. Each shard's
// slice must already be sorted by the merge column (shards bake
// ORDER BY into their SQL; see docs/plans/direct-to-compute.md §3).
//
// Supported merge keys:
//
//	"line_num"       → ascending by ResultRow.LineNum
//	"line_num DESC"  → descending by LineNum
//	"raw"            → ascending by ResultRow.Raw
//	"raw DESC"       → descending by Raw
//
// Unknown keys fall back to a deterministic concat (shards in map
// iteration order) so a bad key is loud but doesn't crash — the
// planner is the source of truth for mergeKey, and a mismatch there
// surfaces as "rows came out in wrong order," not as a 500.
func mergeRows(perShard map[string][]*pb.ResultRow, mergeKey string) []string {
	merged := mergeRowsFull(perShard, mergeKey)
	out := make([]string, len(merged))
	for i, r := range merged {
		out[i] = r.Raw
	}
	return out
}

// mergeRowsFull is the row-preserving counterpart to mergeRows. Used
// by LocalComputeJob to expose merged rows (with line_num intact) to
// callers that haven't migrated to LineStream yet — see
// LocalComputeJob.MergedRows.
func mergeRowsFull(perShard map[string][]*pb.ResultRow, mergeKey string) []*pb.ResultRow {
	key, desc := parseMergeKey(mergeKey)

	var less func(a, b *pb.ResultRow) bool
	switch key {
	case "line_num":
		less = func(a, b *pb.ResultRow) bool { return a.LineNum < b.LineNum }
	case "raw":
		less = func(a, b *pb.ResultRow) bool { return a.Raw < b.Raw }
	default:
		var out []*pb.ResultRow
		for _, rows := range perShard {
			out = append(out, rows...)
		}
		return out
	}
	if desc {
		orig := less
		less = func(a, b *pb.ResultRow) bool { return orig(b, a) }
	}

	cursors := make([]*shardCursor, 0, len(perShard))
	for _, rows := range perShard {
		if len(rows) > 0 {
			cursors = append(cursors, &shardCursor{rows: rows})
		}
	}
	h := &rowHeap{cursors: cursors, less: less}
	heap.Init(h)

	out := make([]*pb.ResultRow, 0)
	for h.Len() > 0 {
		c := heap.Pop(h).(*shardCursor)
		out = append(out, c.rows[c.idx])
		c.idx++
		if c.idx < len(c.rows) {
			heap.Push(h, c)
		}
	}
	return out
}

// parseMergeKey splits "line_num DESC" → ("line_num", true).
func parseMergeKey(mk string) (col string, desc bool) {
	mk = strings.TrimSpace(mk)
	if u := strings.ToUpper(mk); strings.HasSuffix(u, " DESC") {
		return strings.TrimSpace(mk[:len(mk)-len(" DESC")]), true
	}
	return mk, false
}

// shardCursor walks one shard's pre-sorted row slice.
type shardCursor struct {
	rows []*pb.ResultRow
	idx  int
}

// rowHeap is a container/heap.Interface over shardCursors, ordered by
// the head row of each cursor under the supplied less predicate.
type rowHeap struct {
	cursors []*shardCursor
	less    func(a, b *pb.ResultRow) bool
}

func (h *rowHeap) Len() int { return len(h.cursors) }
func (h *rowHeap) Less(i, j int) bool {
	return h.less(h.cursors[i].rows[h.cursors[i].idx], h.cursors[j].rows[h.cursors[j].idx])
}
func (h *rowHeap) Swap(i, j int)       { h.cursors[i], h.cursors[j] = h.cursors[j], h.cursors[i] }
func (h *rowHeap) Push(x any)          { h.cursors = append(h.cursors, x.(*shardCursor)) }
func (h *rowHeap) Pop() any {
	n := len(h.cursors)
	c := h.cursors[n-1]
	h.cursors = h.cursors[:n-1]
	return c
}
