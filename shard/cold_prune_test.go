package shard

import "testing"

func TestPruneColdPartitions(t *testing.T) {
	const planned = "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND ts >= CAST(? AS TIMESTAMP) AND ts <= CAST(? AS TIMESTAMP) AND namespace = ?"
	args := []string{"1", "2026-10-14 12:00:00", "2026-10-14 12:05:00", "weft"}

	cases := []struct {
		name string
		sql  string
		args []string
		want string
	}{
		{
			name: "both bounds get a partition bound, a day looser on the low side",
			sql:  planned,
			args: args,
			want: "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND (ts >= CAST(? AS TIMESTAMP) AND _part > '2026-10-13 12:00:00') AND (ts <= CAST(? AS TIMESTAMP) AND _part <= '2026-10-14 12:05:00') AND namespace = ?",
		},
		{
			name: "a ? inside a string literal is not a placeholder",
			sql:  "SELECT raw, ts FROM logs WHERE raw LIKE '%why?%' AND ts >= CAST(? AS TIMESTAMP)",
			args: []string{"2026-10-14 12:00:00"},
			want: "SELECT raw, ts FROM logs WHERE raw LIKE '%why?%' AND (ts >= CAST(? AS TIMESTAMP) AND _part > '2026-10-13 12:00:00')",
		},
		{
			name: "microseconds survive",
			sql:  "SELECT raw, ts FROM logs WHERE ts <= CAST(? AS TIMESTAMP)",
			args: []string{"2026-10-14 12:05:00.123456"},
			want: "SELECT raw, ts FROM logs WHERE (ts <= CAST(? AS TIMESTAMP) AND _part <= '2026-10-14 12:05:00.123456')",
		},
		{name: "no time bound, unchanged", sql: "SELECT raw, ts FROM logs WHERE namespace = ?", args: []string{"weft"}, want: "SELECT raw, ts FROM logs WHERE namespace = ?"},
		{name: "a bound that is not a time, unchanged", sql: planned, args: []string{"1", "yesterday", "2026-10-14 12:05:00", "weft"}, want: "SELECT raw, ts FROM logs WHERE customer_id = CAST(? AS BIGINT) AND ts >= CAST(? AS TIMESTAMP) AND (ts <= CAST(? AS TIMESTAMP) AND _part <= '2026-10-14 12:05:00') AND namespace = ?"},
		{name: "too few args, unchanged", sql: planned, args: []string{"1"}, want: planned},
		{name: "a longer column name is not ts", sql: "SELECT raw, ts FROM logs WHERE pts >= CAST(? AS TIMESTAMP)", args: []string{"2026-10-14 12:00:00"}, want: "SELECT raw, ts FROM logs WHERE pts >= CAST(? AS TIMESTAMP)"},
	}
	for _, c := range cases {
		if got := PruneColdPartitions(c.sql, c.args); got != c.want {
			t.Errorf("%s:\n got: %s\nwant: %s", c.name, got, c.want)
		}
	}
}
