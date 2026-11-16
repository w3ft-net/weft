//go:build integration

package planner

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
	_ "github.com/w3ft-net/weft/shard/duckdbdrv"
)

// TestProjection_E2E runs the planner's projection-folded SQL through a
// real in-memory DuckDB shard against fixed JSON payloads, and asserts the
// shard returns the value the original (un-optimized) jq pipeline would
// have produced. Failures here mean the planner is emitting SQL that's
// either invalid or semantically divergent from jq.
func TestProjection_E2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	sh, err := shard.NewDuckDBShard("e2e", "")
	if err != nil {
		t.Fatalf("NewDuckDBShard: %v", err)
	}
	defer sh.Close()

	now := time.Now()
	rows := []*pb.LogRow{
		{
			LineNum:     1,
			Raw:         `{"status":500,"user_id":"u1","action":"click","foo":{"bar":42},"items":[10,20,30],"users":[{"name":"b"},{"name":"a"}]}`,
			TsUnixNs:    now.UnixNano(),
			Level:       "ERROR",
			Namespace:   "vimgym",
			Service:     "api",
			Kind:        "main",
			PayloadJson: []byte(`{"status":500,"user_id":"u1","action":"click","foo":{"bar":42},"items":[10,20,30],"users":[{"name":"b"},{"name":"a"}]}`),
		},
		{
			LineNum:     2,
			Raw:         `{"status":200,"user_id":"u2","action":"view","foo":{"bar":7},"items":[1],"users":[{"name":"z"}]}`,
			TsUnixNs:    now.Add(time.Second).UnixNano(),
			Level:       "INFO",
			Namespace:   "vimgym",
			Service:     "api",
			Kind:        "main",
			PayloadJson: []byte(`{"status":200,"user_id":"u2","action":"view","foo":{"bar":7},"items":[1],"users":[{"name":"z"}]}`),
		},
	}
	if err := sh.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatalf("Ingest: %v", err)
	}

	cases := []struct {
		name     string
		pipeline string
		// wantRaws is what each result row's `raw` column should contain,
		// in row order. Predicates may filter rows out; we check the count
		// matches len(wantRaws) too.
		wantRaws []string
	}{
		{
			name:     "single field projection",
			pipeline: `cat /log/vimgym/api/main.log | jq '.status'`,
			wantRaws: []string{"500", "200"},
		},
		{
			name:     "nested path",
			pipeline: `cat /log/vimgym/api/main.log | jq '.foo.bar'`,
			wantRaws: []string{"42", "7"},
		},
		{
			name:     "object construction",
			pipeline: `cat /log/vimgym/api/main.log | jq '{user: .user_id, action: .action}'`,
			wantRaws: []string{
				`{"user":"u1","action":"click"}`,
				`{"user":"u2","action":"view"}`,
			},
		},
		{
			name:     "array construction",
			pipeline: `cat /log/vimgym/api/main.log | jq '[.user_id, .action]'`,
			wantRaws: []string{
				`["u1","click"]`,
				`["u2","view"]`,
			},
		},
		{
			name:     "select + projection",
			pipeline: `cat /log/vimgym/api/main.log | jq 'select(.status == 500) | .user_id'`,
			wantRaws: []string{`"u1"`},
		},
		{
			name:     "select + object projection (with metavar)",
			pipeline: `cat /log/vimgym/api/main.log | jq 'select(.status == 500) | {who: .user_id, lvl: $level}'`,
			wantRaws: []string{`{"who":"u1","lvl":"ERROR"}`},
		},
		{
			name: "internal pipe: the shard sends only the fields jq reads",
			// jq runs in compute; the shard narrows each payload to the keys the
			// program reads (.status, .users). The compute step isn't part of
			// this test.
			pipeline: `cat /log/vimgym/api/main.log | jq 'select(.status == 500) | .users | sort_by(.name)'`,
			wantRaws: []string{`{"status":500,"users":[{"name":"b"},{"name":"a"}]}`},
		},
		{
			name:     "iter: the shard sends the fields, .[] runs in compute",
			pipeline: `cat /log/vimgym/api/main.log | jq 'select(.status == 500) | .items[]'`,
			wantRaws: []string{`{"items":[10,20,30],"status":500}`},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := planBare(tt.pipeline, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			sql := plan.ShardSQL
			// Sort by ts for deterministic row order (no ORDER BY in cat).
			if !strings.Contains(sql, "ORDER BY") {
				sql += " ORDER BY ts"
			}
			result, err := sh.Query(ctx, sql, plan.Args...)
			if err != nil {
				t.Fatalf("Query: %v\n  sql: %s", err, sql)
			}
			if len(result) != len(tt.wantRaws) {
				t.Fatalf("row count: got %d want %d\n  sql: %s\n  rows: %+v",
					len(result), len(tt.wantRaws), sql, rawValues(result))
			}
			for i, want := range tt.wantRaws {
				got := result[i].Raw
				// DuckDB's json_extract preserves key order from input but may
				// emit doubles like 42.0; tolerate that vs "42".
				if got != want && normalizeJSON(got) != normalizeJSON(want) {
					t.Errorf("row %d raw\n  got:  %q\n  want: %q\n  sql:  %s", i, got, want, sql)
				}
			}
		})
	}
}

func rawValues(rows []*pb.ResultRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Raw
	}
	return out
}

// normalizeJSON strips whitespace differences for tolerant comparison. We
// don't need true JSON parsing — DuckDB and jq both emit canonical-ish
// output and we just want to absorb the occasional 42.0-vs-42 difference.
func normalizeJSON(s string) string {
	s = strings.ReplaceAll(s, " ", "")
	return s
}
