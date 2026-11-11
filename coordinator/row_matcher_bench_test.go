package coordinator

import (
	"fmt"
	"testing"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

// liveRows is a stream like the coordinator's: rows from many customers, of
// which a subscription wants one customer's.
func liveRows() []Row {
	rows := make([]Row, 0, 1000)
	for i := 0; i < 1000; i++ {
		rows = append(rows, Row{LogRow: &pb.LogRow{
			Namespace: fmt.Sprintf("ns%d", i%4),
			Service:   fmt.Sprintf("svc%d", i%8),
			Kind:      "main",
			Env:       "prod",
			Raw:       "2026-11-11T10:00:00Z INFO request path=/api/v1/items status=200",
		}, Added: shape.Added{Customer: int64(1 + i%50)}})
	}
	return rows
}

var grepError = []planner.GrepPattern{{Pattern: "status=500"}}

// BenchmarkRowMatcher is the live-tail hot path: every ingested row is offered
// to every subscription, and most belong to someone else.
func BenchmarkRowMatcher(b *testing.B) {
	rows := liveRows()
	one := shape.Set{{Customer: 7, Namespace: shape.Value("ns1"), Service: shape.Value("svc3"), Kind: shape.Value("main"), Env: shape.Any()}}
	var many shape.Set
	for i := 0; i < 20; i++ {
		many = append(many, shape.Coord{Customer: 7, Namespace: shape.Value(fmt.Sprintf("ns%d", i%4)), Service: shape.Value(fmt.Sprintf("svc%d", i)), Kind: shape.Value("main")})
	}
	for _, tc := range []struct {
		name string
		m    *RowMatcher
	}{
		{"one tuple", &RowMatcher{Coords: one}},
		{"one tuple with a grep", &RowMatcher{Coords: one, Patterns: grepError}},
		{"twenty tuples", &RowMatcher{Coords: many}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			n := 0
			for i := 0; i < b.N; i++ {
				if tc.m.Match(rows[i%len(rows)]) {
					n++
				}
			}
			_ = n
		})
	}
}
