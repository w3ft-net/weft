//go:build test

package coordtest

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/w3ft-net/weft/logdb"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// NamespaceQueries reads a namespace as a whole (cat /log/weft.log) and a
// service of it, over one shard that holds the sub-apps of the namespace
// "weft" as weftd ships them, and a namespace beside it.
func NamespaceQueries(t *testing.T, newSubject func(t *testing.T) Subject) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, *queryFixture)
	}{
		// Every sub-app of the namespace comes back, and the other namespace
		// does not.
		{"All", func(t *testing.T, f *queryFixture) {
			d := f.exec(t, "cat /log/weft.log", f.full())
			if len(d.Rows) != 15 {
				t.Errorf("expected 15 rows for a namespace query (5 coordinator, 5 shard, 5 metrics), got %d", len(d.Rows))
			}
			counts := map[string]int{}
			for _, r := range d.Rows {
				switch {
				case strings.Contains(r.Raw, "coordinator:"):
					counts["coordinator/main"]++
				case strings.Contains(r.Raw, "shard:"):
					counts["shard/main"]++
				case strings.Contains(r.Raw, "collector"):
					counts["coordinator/metrics"]++
				case strings.Contains(r.Raw, "grantcentral"):
					counts["grantcentral"]++
				}
			}
			for _, want := range []string{"coordinator/main", "shard/main", "coordinator/metrics"} {
				if counts[want] != 5 {
					t.Errorf("expected 5 rows for %s, got %d", want, counts[want])
				}
			}
			if counts["grantcentral"] > 0 {
				t.Errorf("grantcentral rows leaked into the weft namespace query")
			}
		}},
		{"ServiceMain", func(t *testing.T, f *queryFixture) {
			d := f.exec(t, "cat /log/weft/coordinator/main.log", f.full())
			if len(d.Rows) != 5 {
				t.Errorf("expected 5 rows for a service query, got %d", len(d.Rows))
			}
			for _, r := range d.Rows {
				if !strings.Contains(r.Raw, "coordinator:") {
					t.Errorf("unexpected raw in a service query: %q", r.Raw)
				}
			}
		}},
		{"ServiceMetrics", func(t *testing.T, f *queryFixture) {
			d := f.exec(t, "cat /log/weft/coordinator/metrics.log", f.full())
			if len(d.Rows) != 5 {
				t.Errorf("expected 5 rows for a metrics query, got %d", len(d.Rows))
			}
			for _, r := range d.Rows {
				if !strings.Contains(r.Raw, "collector") {
					t.Errorf("unexpected raw in a metrics query: %q", r.Raw)
				}
			}
		}},
		{"HeadTail", func(t *testing.T, f *queryFixture) {
			for _, line := range []string{"head -3 /log/weft.log", "tail -3 /log/weft.log"} {
				if d := f.exec(t, line, f.full()); len(d.Rows) != 3 {
					t.Errorf("%s: expected 3 rows, got %d", line, len(d.Rows))
				}
			}
		}},
		{"FilterAcrossSubApps", func(t *testing.T, f *queryFixture) {
			if d := f.exec(t, `cat /log/weft.log | grep coordinator`, f.full()); len(d.Rows) != 5 {
				t.Errorf("expected 5 rows matching 'coordinator', got %d", len(d.Rows))
			}
			if d := f.exec(t, `cat /log/weft.log | awk 'INSTANCE == "i-shard-001"'`, f.full()); len(d.Rows) != 5 {
				t.Errorf("expected 5 rows from i-shard-001, got %d", len(d.Rows))
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, newNamespaceFixture(t, newSubject(t))) })
	}
}

// newNamespaceFixture is one shard of the namespace weft: 5 rows of
// weft/coordinator main, 5 of weft/shard main, 5 JSON metric rows of
// weft/coordinator, and one row of the namespace grantcentral.
func newNamespaceFixture(t *testing.T, s Subject) *queryFixture {
	t.Helper()
	f := newBareFixture(t, s)
	for _, ns := range []string{"weft", "grantcentral"} {
		n, err := s.Sources.FindOrCreateNamespace(s.Owner, ns)
		if err != nil {
			t.Fatal(err)
		}
		f.namespaces[ns] = n.ID
	}
	var rows []*pb.LogRow
	for i := 0; i < 5; i++ {
		at := f.t0.Add(time.Duration(i) * time.Minute).UnixNano()
		rows = append(rows,
			&pb.LogRow{TsUnixNs: at, Namespace: "weft", Service: "coordinator", Kind: "main", Raw: "coordinator: handling query", Level: "INFO", Instance: "i-coord-001"},
			&pb.LogRow{TsUnixNs: at, Namespace: "weft", Service: "shard", Kind: "main", Raw: "shard: ingesting batch", Level: "INFO", Instance: "i-shard-001"},
			&pb.LogRow{TsUnixNs: at, Namespace: "weft", Service: "coordinator", Kind: "metrics", Raw: `{"collector":"cpu","value":42}`, Level: "METRIC", Instance: "i-coord-001",
				PayloadJson: payload(map[string]any{"collector": "cpu", "value": 42})},
		)
	}
	rows = append(rows, &pb.LogRow{TsUnixNs: f.t0.UnixNano(), Namespace: "grantcentral", Raw: "grantcentral: should not appear", Level: "INFO", Instance: "i-gc-001"})
	for _, r := range rows {
		if s.Stamp != nil {
			s.Stamp(r)
		}
	}
	live, err := shard.NewDuckDBShard("ns-shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	if err := live.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	f.registerServing(t, &logdb.Shard{
		ShardRow: logdb.ShardRow{ID: "ns-shard-0", WindowStart: f.t0, WindowEnd: f.t1, State: logdb.StateIngesting, CreatedAt: time.Now()},
		Live:     live,
	}, "weft/coordinator", "weft/shard", "grantcentral")
	return f
}
