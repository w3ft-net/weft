package shard

import "testing"

func TestPruneColdPartitions(t *testing.T) {
	const planned = "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND ts >= CAST(? AS TIMESTAMP) AND ts <= CAST(? AS TIMESTAMP) AND namespace = ?"
	args := []string{"1", "2026-10-14 12:00:00", "2026-10-14 12:05:00", "weft"}

	const legacy = "s3://b/x/shard.attempt-1/**/*.parquet"
	cases := []struct {
		name string
		sql  string
		args []string
		path string
		want string
	}{
		{
			path: legacy,
			name: "both bounds get a partition bound, a day looser on the low side",
			sql:  planned,
			args: args,
			want: "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND (ts >= CAST(? AS TIMESTAMP) AND _part > '2026-10-13 12:00:00') AND (ts <= CAST(? AS TIMESTAMP) AND _part <= '2026-10-14 12:05:00') AND namespace = ?",
		},
		{
			path: legacy,
			name: "a ? inside a string literal is not a placeholder",
			sql:  "SELECT raw, ts FROM logs WHERE raw LIKE '%why?%' AND ts >= CAST(? AS TIMESTAMP)",
			args: []string{"2026-10-14 12:00:00"},
			want: "SELECT raw, ts FROM logs WHERE raw LIKE '%why?%' AND (ts >= CAST(? AS TIMESTAMP) AND _part > '2026-10-13 12:00:00')",
		},
		{
			path: legacy,
			name: "microseconds survive",
			sql:  "SELECT raw, ts FROM logs WHERE ts <= CAST(? AS TIMESTAMP)",
			args: []string{"2026-10-14 12:05:00.123456"},
			want: "SELECT raw, ts FROM logs WHERE (ts <= CAST(? AS TIMESTAMP) AND _part <= '2026-10-14 12:05:00.123456')",
		},
		{
			// A sized archive: every key has its own width in _w, so the low bound is
			// that a row's bucket ends after the bound, whatever the width. The high
			// bound is the same as in any archive.
			path: "s3://b/x/shard.attempt-1/sized/**/*.parquet",
			name: "a sized archive bounds the low side by each key's own bucket width",
			sql:  planned,
			args: args,
			want: "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND (ts >= CAST(? AS TIMESTAMP) AND CAST(_part AS TIMESTAMP) + to_minutes(CAST(_w AS INTEGER)) > TIMESTAMP '2026-10-14 12:00:00') AND (ts <= CAST(? AS TIMESTAMP) AND _part <= '2026-10-14 12:05:00') AND namespace = ?",
		},
		{path: legacy, name: "no time bound, unchanged", sql: "SELECT raw, ts FROM logs WHERE namespace = ?", args: []string{"weft"}, want: "SELECT raw, ts FROM logs WHERE namespace = ?"},
		{path: legacy, name: "a bound that is not a time, unchanged", sql: planned, args: []string{"1", "yesterday", "2026-10-14 12:05:00", "weft"}, want: "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND ts >= CAST(? AS TIMESTAMP) AND (ts <= CAST(? AS TIMESTAMP) AND _part <= '2026-10-14 12:05:00') AND namespace = ?"},
		{path: legacy, name: "too few args, unchanged", sql: planned, args: []string{"1"}, want: planned},
		{path: legacy, name: "a longer column name is not ts", sql: "SELECT raw, ts FROM logs WHERE pts >= CAST(? AS TIMESTAMP)", args: []string{"2026-10-14 12:00:00"}, want: "SELECT raw, ts FROM logs WHERE pts >= CAST(? AS TIMESTAMP)"},
		{
			// A known width: the low bound is the start of the bucket holding it
			// (07:10 with 180-minute buckets from the epoch is in the 06:00 one).
			path: "s3://b/x/shard.attempt-1/m180/**/*.parquet",
			name: "a known width pins the low bound to its bucket",
			sql:  "SELECT raw, ts FROM logs WHERE ts >= CAST(? AS TIMESTAMP) AND ts <= CAST(? AS TIMESTAMP)",
			args: []string{"2026-10-27 07:10:00", "2026-10-27 07:20:00"},
			want: "SELECT raw, ts FROM logs WHERE (ts >= CAST(? AS TIMESTAMP) AND _part >= '2026-10-27 06:00:00') AND (ts <= CAST(? AS TIMESTAMP) AND _part <= '2026-10-27 07:20:00')",
		},
	}
	for _, c := range cases {
		if got := PruneColdPartitions(c.sql, c.args, c.path); got != c.want {
			t.Errorf("%s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}
}
