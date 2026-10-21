package compute

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// The two steps of the trace list (see planner/traces.go).

// traceIDsAggregator keeps the newest planner.TraceListLimit trace IDs of the
// ones each shard named, by their newest line on any shard.
type traceIDsAggregator struct {
	sink func(*pb.ResultRow) bool
	last map[string]int64
	err  error
}

func (a *traceIDsAggregator) push(row *pb.ResultRow) bool {
	var p struct {
		TraceID string `json:"trace_id"`
		LastNs  int64  `json:"last_ns"`
	}
	if err := json.Unmarshal([]byte(row.Raw), &p); err != nil {
		a.err = fmt.Errorf("compute: trace id row: %w", err)
		return false
	}
	if cur, ok := a.last[p.TraceID]; !ok || p.LastNs > cur {
		a.last[p.TraceID] = p.LastNs
	}
	return true
}

func (a *traceIDsAggregator) failure() error { return a.err }

func (a *traceIDsAggregator) flush() {
	if a.err != nil {
		return
	}
	ids := make([]string, 0, len(a.last))
	for id := range a.last {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if a.last[ids[i]] != a.last[ids[j]] {
			return a.last[ids[i]] > a.last[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if len(ids) > planner.TraceListLimit {
		ids = ids[:planner.TraceListLimit]
	}
	for _, id := range ids {
		out, _ := json.Marshal(struct {
			TraceID string `json:"trace_id"`
			LastNs  int64  `json:"last_ns"`
		}{id, a.last[id]})
		if !a.sink(&pb.ResultRow{Raw: string(out)}) {
			return
		}
	}
}

// tracePartial is one shard's summary of one trace.
type tracePartial struct {
	TraceID  string   `json:"trace_id"`
	FirstNs  int64    `json:"first_ns"`
	LastNs   int64    `json:"last_ns"`
	Lines    int64    `json:"lines"`
	Errors   int64    `json:"errors"`
	Levels   int64    `json:"levels"`
	Services []string `json:"services"`
	Root     *string  `json:"root"`
	RootNs   *int64   `json:"root_ns"`
}

type traceGroup struct {
	first, last   int64
	lines, errors int64
	levels        int64
	services      map[string]bool
	root          string
	rootNs        int64
}

// TraceSummary is one line of a bare `trace`.
type TraceSummary struct {
	TraceID  string   `json:"trace_id"`
	StartNs  int64    `json:"start_ns"`
	EndNs    int64    `json:"end_ns"`
	DurNs    int64    `json:"dur_ns"`
	Lines    int64    `json:"lines"`
	Errors   *int64   `json:"errors"` // null when no line carried a level
	Services []string `json:"services"`
	Root     string   `json:"root,omitempty"`
}

// traceListAggregator merges the shards' summaries of each trace.
type traceListAggregator struct {
	sink   func(*pb.ResultRow) bool
	groups map[string]*traceGroup
	err    error
}

func (a *traceListAggregator) push(row *pb.ResultRow) bool {
	var p tracePartial
	if err := json.Unmarshal([]byte(row.Raw), &p); err != nil {
		a.err = fmt.Errorf("compute: trace summary row: %w", err)
		return false
	}
	g := a.groups[p.TraceID]
	if g == nil {
		g = &traceGroup{first: p.FirstNs, last: p.LastNs, services: map[string]bool{}}
		a.groups[p.TraceID] = g
	}
	g.first, g.last = min(g.first, p.FirstNs), max(g.last, p.LastNs)
	g.lines += p.Lines
	g.errors += p.Errors
	g.levels += p.Levels
	for _, s := range p.Services {
		if s != "" {
			g.services[s] = true
		}
	}
	// Several services can each have a parentless span (a client call and the
	// server handling it); the earliest is the one that started the request.
	if p.Root != nil && *p.Root != "" && p.RootNs != nil && (g.root == "" || *p.RootNs < g.rootNs) {
		g.root, g.rootNs = *p.Root, *p.RootNs
	}
	return true
}

func (a *traceListAggregator) failure() error { return a.err }

func (a *traceListAggregator) flush() {
	if a.err != nil {
		return
	}
	ids := make([]string, 0, len(a.groups))
	for id := range a.groups {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		gi, gj := a.groups[ids[i]], a.groups[ids[j]]
		if gi.last != gj.last {
			return gi.last > gj.last
		}
		return ids[i] < ids[j]
	})
	for _, id := range ids {
		g := a.groups[id]
		sum := TraceSummary{
			TraceID: id, StartNs: g.first, EndNs: g.last, DurNs: g.last - g.first,
			Lines: g.lines, Services: make([]string, 0, len(g.services)), Root: g.root,
		}
		for s := range g.services {
			sum.Services = append(sum.Services, s)
		}
		sort.Strings(sum.Services)
		if g.levels > 0 {
			errs := g.errors
			sum.Errors = &errs
		}
		out, err := json.Marshal(sum)
		if err != nil {
			a.err = err
			return
		}
		if !a.sink(&pb.ResultRow{Raw: string(out)}) {
			return
		}
	}
}
