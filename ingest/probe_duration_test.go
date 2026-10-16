package ingest

import "testing"

// compute logs both a human duration and the same value in ns on every
// "span done" line. Highlighting either must yield a rule; the bug was that
// the human form could never match because the checker compares the mark text
// to the parsed ns integer.
func TestProbeDurationHighlightedAsHumanForm(t *testing.T) {
	lines := []string{
		"2026/10/16 09:42:36 span start name=compute.ExecuteQueryStream trace_id=0d11 span_id=f20a parent_span_id=4553",
		"2026/10/16 09:42:41 cold shard shard-1: lambda returned 500: query failed: IO Error",
		"LINE 1: ... raw, ts FROM read_parquet('s3://x')",
		"^",
		"2026/10/16 09:42:41 span done name=compute.ExecuteQueryStream trace_id=0d11 span_id=f20a parent_span_id=4553 duration=5.015996546s duration_ns=5015996546 status=ok",
		"2026/10/16 09:42:58 span done name=compute.ExecuteQueryStream trace_id=5133 span_id=7e40 parent_span_id=4f5b duration=1.147619075s duration_ns=1147619075 status=ok",
	}
	for _, text := range []string{"5.015996546s", "5015996546"} {
		t.Run(text, func(t *testing.T) {
			res := probe(t, ProbeRequest{Field: ProbeDurationNs, Lines: lines, Marks: []ProbeMark{markAt(t, lines, 4, text)}})
			wantValues(t, res, map[int]string{4: "5015996546", 5: "1147619075"})
		})
	}
}
