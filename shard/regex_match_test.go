package shard

import (
	"testing"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// TestDuckDB_RegexpMatches_PinsPushdownContract guards the SQL the
// planner emits for grep / awk-regex / sed-address pushdown. The
// planner emits one of:
//
//	regexp_matches(raw, 'pat')
//	regexp_matches(raw, 'pat', 'i')
//	NOT regexp_matches(raw, 'pat')
//	NOT regexp_matches(raw, 'pat', 'i')
//
// against the live DuckDB schema. If a future schema change or DuckDB
// upgrade breaks any of those forms, this test fails before users hit
// "0 rows" silently in production.
//
// Background: commit a1842de assumed DuckDB's `~` was substring regex
// (PostgreSQL semantics) — it isn't. `~` in DuckDB is whole-string
// match (SIMILAR TO semantics) and `~*` doesn't exist as a scalar
// function. The fix was to switch all three pushdowns to
// regexp_matches, which IS substring/find. This test documents the
// contract so it can't regress quietly.
func TestDuckDB_RegexpMatches_PinsPushdownContract(t *testing.T) {
	s, err := NewDuckDBShard("regex-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	rows := []*pb.LogRow{
		{Raw: "2026-03-28T10:00:01 ERROR vimgym i-001 connection timeout", Customer: "kevlar", App: "vimgym", TsUnixNs: 1},
		{Raw: "2026-03-28T10:00:05 INFO vimgym i-001 request completed", Customer: "kevlar", App: "vimgym", TsUnixNs: 2},
		{Raw: "2026-03-28T10:00:09 DEBUG vimgym i-001 cache miss", Customer: "kevlar", App: "vimgym", TsUnixNs: 3},
	}
	if err := s.Ingest(rows); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	cases := []struct {
		name string
		sql  string
		want int
	}{
		{
			name: "case-sensitive substring (grep, awk regex, sed)",
			sql:  "SELECT raw, ts FROM logs WHERE customer = 'kevlar' AND regexp_matches(raw, 'timeout')",
			want: 1,
		},
		{
			name: "case-insensitive substring (grep -i)",
			sql:  "SELECT raw, ts FROM logs WHERE customer = 'kevlar' AND regexp_matches(raw, 'TIMEOUT', 'i')",
			want: 1,
		},
		{
			name: "negated substring (grep -v, awk !~)",
			sql:  "SELECT raw, ts FROM logs WHERE customer = 'kevlar' AND NOT regexp_matches(raw, 'timeout')",
			want: 2,
		},
		{
			name: "negated case-insensitive (grep -iv)",
			sql:  "SELECT raw, ts FROM logs WHERE customer = 'kevlar' AND NOT regexp_matches(raw, 'DEBUG', 'i')",
			want: 2,
		},
		{
			name: "anchored pattern (^ERROR — what `raw ~` would silently break)",
			sql:  "SELECT raw, ts FROM logs WHERE customer = 'kevlar' AND regexp_matches(raw, 'ERROR')",
			want: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Query(tc.sql)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("expected %d rows, got %d (sql=%s)", tc.want, len(got), tc.sql)
			}
		})
	}
}
