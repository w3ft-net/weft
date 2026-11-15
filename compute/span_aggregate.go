package compute

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"

	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

type spanPartial struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Bucket    *int64 `json:"bucket"`
	Count     int64  `json:"count"`
	Errors    int64  `json:"errors"`
	Levels    int64  `json:"levels"` // rows that carried a level
	TotalNs   *int64 `json:"total_ns"`
	MinNs     *int64 `json:"min_ns"`
	MaxNs     *int64 `json:"max_ns"`
}

type spanKey struct{ namespace, service, name string }

type spanGroup struct {
	count, errors int64
	levels        int64
	total         float64
	min, max      int64
	buckets       map[int64]int64
}

type spanSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Service   string `json:"service,omitempty"`
	Count     int64  `json:"count"`
	Errors    *int64 `json:"errors"` // null when no row carried a level
	TotalNs   int64  `json:"total_ns"`
	AvgNs     int64  `json:"avg_ns"`
	P50Ns     int64  `json:"p50_ns"`
	P99Ns     int64  `json:"p99_ns"`
	MinNs     int64  `json:"min_ns"`
	MaxNs     int64  `json:"max_ns"`
}

type spanAggregator struct {
	sink   func(*pb.ResultRow) bool
	groups map[spanKey]*spanGroup
	err    error
}

func (a *spanAggregator) push(row *pb.ResultRow) bool {
	var p spanPartial
	if err := json.Unmarshal([]byte(row.Raw), &p); err != nil {
		a.err = fmt.Errorf("compute: span aggregate row: %w", err)
		return false
	}
	k := spanKey{p.Namespace, p.Service, p.Name}
	g := a.groups[k]
	if g == nil {
		g = &spanGroup{buckets: map[int64]int64{}}
		a.groups[k] = g
	}
	g.count += p.Count
	g.errors += p.Errors
	g.levels += p.Levels
	if p.Count > 0 && p.TotalNs != nil && p.MinNs != nil && p.MaxNs != nil {
		g.total += float64(*p.TotalNs)
		if g.count == p.Count || *p.MinNs < g.min {
			g.min = *p.MinNs
		}
		if *p.MaxNs > g.max {
			g.max = *p.MaxNs
		}
		if p.Bucket != nil {
			g.buckets[*p.Bucket] += p.Count
		}
	}
	return true
}

func (a *spanAggregator) failure() error { return a.err }

func (a *spanAggregator) flush() {
	if a.err != nil {
		return
	}
	keys := make([]spanKey, 0, len(a.groups))
	for k := range a.groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		x, y := keys[i], keys[j]
		if x.name != y.name {
			return x.name < y.name
		}
		if x.namespace != y.namespace {
			return x.namespace < y.namespace
		}
		return x.service < y.service
	})
	for _, k := range keys {
		g := a.groups[k]
		sum := spanSummary{Name: k.name, Namespace: k.namespace, Service: k.service, Count: g.count}
		if g.levels > 0 {
			sum.Errors = &g.errors
		}
		if g.count > 0 {
			sum.TotalNs = int64(g.total)
			sum.AvgNs = int64(g.total / float64(g.count))
			sum.MinNs, sum.MaxNs = g.min, g.max
			sum.P50Ns = g.percentile(0.50)
			sum.P99Ns = g.percentile(0.99)
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

func (g *spanGroup) percentile(p float64) int64 {
	buckets := make([]int64, 0, len(g.buckets))
	var n int64
	for b, c := range g.buckets {
		buckets = append(buckets, b)
		n += c
	}
	if n == 0 {
		return 0
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })
	rank := int64(math.Ceil(p * float64(n)))
	if rank < 1 {
		rank = 1
	}
	var seen int64
	for _, b := range buckets {
		seen += g.buckets[b]
		if seen >= rank {
			v := int64(math.Round(planner.SpanBucketValue(b)))
			return min(max(v, g.min), g.max)
		}
	}
	return g.max
}

type spanRowAggregator struct {
	fn   string
	sink func(*pb.ResultRow) bool
	rows []spanOccurrenceRow
	err  error
}

type spanOccurrenceRow struct {
	raw   string
	ts    int64
	durNs int64
}

func (a *spanRowAggregator) push(row *pb.ResultRow) bool {
	var o struct {
		TsNs  int64 `json:"ts_ns"`
		DurNs int64 `json:"dur_ns"`
	}
	if err := json.Unmarshal([]byte(row.Raw), &o); err != nil {
		a.err = fmt.Errorf("compute: span row: %w", err)
		return false
	}
	a.rows = append(a.rows, spanOccurrenceRow{raw: row.Raw, ts: o.TsNs, durNs: o.DurNs})
	return true
}

func (a *spanRowAggregator) failure() error { return a.err }

func (a *spanRowAggregator) flush() {
	if a.err != nil {
		return
	}
	if a.fn == planner.AggregateSpanSamples {
		sort.SliceStable(a.rows, func(i, j int) bool { return a.rows[i].durNs > a.rows[j].durNs })
	} else {
		sort.SliceStable(a.rows, func(i, j int) bool { return a.rows[i].ts < a.rows[j].ts })
	}
	for _, r := range a.rows {
		if !a.sink(&pb.ResultRow{Raw: r.raw}) {
			return
		}
	}
}
