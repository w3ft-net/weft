package chart

import (
	"strings"
	"testing"
	"time"
)

func TestParsePipeArgs_Defaults(t *testing.T) {
	got, msg := parsePipeArgs([]string{})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if got.Kind != "bar" {
		t.Errorf("default Kind: got %q, want bar", got.Kind)
	}
	if got.Aggregate != "identity" {
		t.Errorf("default aggregate (no bucket): got %q, want identity", got.Aggregate)
	}
}

func TestParsePipeArgs_BucketImpliesLine(t *testing.T) {
	got, msg := parsePipeArgs([]string{"--bucket", "1m", "-x", ".ts", "-y", ".v"})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if got.Kind != "line" {
		t.Errorf("bucket should default kind to line, got %q", got.Kind)
	}
	if got.Bucket != time.Minute {
		t.Errorf("Bucket: got %v, want 1m", got.Bucket)
	}
	if got.Aggregate != "count" {
		t.Errorf("bucket should default aggregate to count when y present, got %q", got.Aggregate)
	}
}

func TestParsePipeArgs_TypeOverride(t *testing.T) {
	got, msg := parsePipeArgs([]string{"--bucket", "1m", "-t", "bar"})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if got.Kind != "bar" {
		t.Errorf("-t should override bucket default, got %q", got.Kind)
	}
}

func TestParsePipeArgs_RejectsMixedAddressing(t *testing.T) {
	_, msg := parsePipeArgs([]string{"-x", "1", "-y", ".latency"})
	if msg == "" {
		t.Fatal("expected error for mixed positional + JSON-path")
	}
	if !strings.Contains(msg, "must all be positional or all") {
		t.Errorf("error message: got %q", msg)
	}
}

func TestParsePipeArgs_AggregateValidation(t *testing.T) {
	_, msg := parsePipeArgs([]string{"-a", "median"})
	if msg == "" || !strings.Contains(msg, "unknown --aggregate") {
		t.Errorf("expected unknown aggregate error, got %q", msg)
	}
	for _, a := range []string{"count", "sum", "avg", "min", "max", "p50", "p95"} {
		_, msg := parsePipeArgs([]string{"-a", a})
		if msg != "" {
			t.Errorf("aggregate %q rejected unexpectedly: %s", a, msg)
		}
	}
}

func TestParsePipeArgs_RejectsPositional(t *testing.T) {
	_, msg := parsePipeArgs([]string{"my-watch"})
	if msg == "" {
		t.Fatal("expected error for stray positional arg in pipe mode")
	}
}

func TestRenderFromStream_DefaultBar(t *testing.T) {
	// `wc -l | chart` — one number per line, last whitespace field.
	args, msg := parsePipeArgs([]string{})
	if msg != "" {
		t.Fatalf("parse: %s", msg)
	}
	out, errMsg := RenderFromStream([]string{"5", "12", "3"}, args, 60, 12)
	if errMsg != "" {
		t.Fatalf("render: %s", errMsg)
	}
	if out == "" {
		t.Fatal("empty output")
	}
}

func TestRenderFromStream_JSONLBucketedLine(t *testing.T) {
	args, msg := parsePipeArgs([]string{"-x", ".ts", "-y", ".v", "--bucket", "1m", "-a", "sum"})
	if msg != "" {
		t.Fatalf("parse: %s", msg)
	}
	lines := []string{
		`{"ts":"2026-01-01T00:00:10Z","v":1}`,
		`{"ts":"2026-01-01T00:00:30Z","v":2}`,
		`{"ts":"2026-01-01T00:01:05Z","v":4}`,
		`{"ts":"2026-01-01T00:01:50Z","v":8}`,
	}
	out, errMsg := RenderFromStream(lines, args, 60, 12)
	if errMsg != "" {
		t.Fatalf("render: %s", errMsg)
	}
	if out == "" {
		t.Fatal("empty output")
	}
}

func TestGroupRows_BucketedSum(t *testing.T) {
	args, _ := parsePipeArgs([]string{"-x", ".ts", "-y", ".v", "--bucket", "1m", "-a", "sum"})
	lines := []string{
		`{"ts":"2026-01-01T00:00:10Z","v":1}`,
		`{"ts":"2026-01-01T00:00:30Z","v":2}`,
		`{"ts":"2026-01-01T00:01:05Z","v":4}`,
		`{"ts":"2026-01-01T00:01:50Z","v":8}`,
	}
	groups, ordered, isTimeX, msg := groupRows(lines, args)
	if msg != "" {
		t.Fatalf("groupRows: %s", msg)
	}
	if !isTimeX {
		t.Error("expected isTimeX=true with --bucket")
	}
	if len(ordered) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(ordered))
	}
	// First Bucket: 1+2 = 3; second Bucket: 4+8 = 12.
	if reduce("sum", groups[ordered[0]]) != 3 {
		t.Errorf("bucket 0 sum: got %v, want 3", reduce("sum", groups[ordered[0]]))
	}
	if reduce("sum", groups[ordered[1]]) != 12 {
		t.Errorf("bucket 1 sum: got %v, want 12", reduce("sum", groups[ordered[1]]))
	}
}

func TestGroupRows_CategoricalBar(t *testing.T) {
	// `cat ... | jq '...' | chart -x .path -y .latency -a sum`
	args, _ := parsePipeArgs([]string{"-x", ".path", "-y", ".latency", "-a", "sum"})
	lines := []string{
		`{"path":"/api","latency":10}`,
		`{"path":"/healthz","latency":1}`,
		`{"path":"/api","latency":15}`,
		`{"path":"/api","latency":5}`,
	}
	groups, ordered, isTimeX, msg := groupRows(lines, args)
	if msg != "" {
		t.Fatalf("groupRows: %s", msg)
	}
	if isTimeX {
		t.Error("categorical x should not be time-x")
	}
	if len(ordered) != 2 {
		t.Fatalf("expected 2 groups, got %d", len(ordered))
	}
	if ordered[0].label != "/api" {
		t.Errorf("first group: got %q, want /api", ordered[0].label)
	}
	if reduce("sum", groups[ordered[0]]) != 30 {
		t.Errorf("/api sum: got %v, want 30", reduce("sum", groups[ordered[0]]))
	}
	if reduce("sum", groups[ordered[1]]) != 1 {
		t.Errorf("/healthz sum: got %v, want 1", reduce("sum", groups[ordered[1]]))
	}
}

func TestReduce(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5}
	cases := []struct {
		name string
		want float64
	}{
		{"count", 5},
		{"sum", 15},
		{"avg", 3},
		{"min", 1},
		{"max", 5},
		{"p50", 3},
		{"p95", 5},
		{"identity", 1},
	}
	for _, c := range cases {
		if got := reduce(c.name, v); got != c.want {
			t.Errorf("reduce(%q): got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParsePipeArgs_TagDefaultsToLine(t *testing.T) {
	got, msg := parsePipeArgs([]string{"-x", ".ts", "-y", ".v", "--tag", ".host"})
	if msg != "" {
		t.Fatalf("unexpected error: %s", msg)
	}
	if got.Kind != "line" {
		t.Errorf("--tag should default kind to line, got %q", got.Kind)
	}
}

func TestParsePipeArgs_TagRejectsBar(t *testing.T) {
	_, msg := parsePipeArgs([]string{"--tag", ".host", "-t", "bar"})
	if msg == "" || !strings.Contains(msg, "only supported for line") {
		t.Errorf("expected --tag+bar rejection, got %q", msg)
	}
}

func TestParsePipeArgs_TagRejectsSparkline(t *testing.T) {
	_, msg := parsePipeArgs([]string{"--tag", ".host", "-t", "sparkline"})
	if msg == "" || !strings.Contains(msg, "only supported for line") {
		t.Errorf("expected --tag+sparkline rejection, got %q", msg)
	}
}

func TestParsePipeArgs_TagJoinsAddressingCheck(t *testing.T) {
	_, msg := parsePipeArgs([]string{"-x", "1", "--tag", ".host"})
	if msg == "" || !strings.Contains(msg, "must all be positional or all") {
		t.Errorf("expected mixed-addressing rejection for -x positional + --tag JSON, got %q", msg)
	}
}

func TestGroupRowsByTag_SplitsIntoSeriesSharingXBuckets(t *testing.T) {
	args, msg := parsePipeArgs([]string{"-x", ".ts", "-y", ".mem_pct", "--tag", ".coordinator_id", "--bucket", "1m", "-a", "avg"})
	if msg != "" {
		t.Fatalf("parse: %s", msg)
	}
	lines := []string{
		`{"ts":"2026-01-01T00:00:10Z","coordinator_id":"i-aaa","mem_pct":40}`,
		`{"ts":"2026-01-01T00:00:10Z","coordinator_id":"i-bbb","mem_pct":60}`,
		`{"ts":"2026-01-01T00:01:10Z","coordinator_id":"i-aaa","mem_pct":44}`,
		`{"ts":"2026-01-01T00:01:10Z","coordinator_id":"i-bbb","mem_pct":64}`,
	}
	tagGroups, isTimeX, errMsg := groupRowsByTag(lines, args)
	if errMsg != "" {
		t.Fatalf("groupRowsByTag: %s", errMsg)
	}
	if !isTimeX {
		t.Error("expected isTimeX=true with --bucket")
	}
	if len(tagGroups) != 2 {
		t.Fatalf("expected 2 tag groups, got %d", len(tagGroups))
	}
	// Alphabetical tag order.
	if tagGroups[0].tag != "i-aaa" || tagGroups[1].tag != "i-bbb" {
		t.Fatalf("expected [i-aaa, i-bbb] order, got [%s, %s]", tagGroups[0].tag, tagGroups[1].tag)
	}
	for _, tg := range tagGroups {
		if len(tg.ordered) != 2 {
			t.Errorf("tag %s: expected 2 time buckets, got %d", tg.tag, len(tg.ordered))
		}
	}
	if got := reduce("avg", tagGroups[0].groups[tagGroups[0].ordered[0]]); got != 40 {
		t.Errorf("i-aaa bucket 0: got %v, want 40", got)
	}
	if got := reduce("avg", tagGroups[1].groups[tagGroups[1].ordered[0]]); got != 60 {
		t.Errorf("i-bbb bucket 0: got %v, want 60", got)
	}
}

func TestGroupRowsByTag_DropsRowsMissingTag(t *testing.T) {
	args, _ := parsePipeArgs([]string{"-x", ".ts", "-y", ".v", "--tag", ".host", "--bucket", "1m"})
	lines := []string{
		`{"ts":"2026-01-01T00:00:10Z","v":1}`, // no "host" field — dropped
		`{"ts":"2026-01-01T00:00:10Z","host":"a","v":2}`,
	}
	tagGroups, _, errMsg := groupRowsByTag(lines, args)
	if errMsg != "" {
		t.Fatalf("groupRowsByTag: %s", errMsg)
	}
	if len(tagGroups) != 1 || tagGroups[0].tag != "a" {
		t.Fatalf("expected exactly one tag group %q, got %+v", "a", tagGroups)
	}
}

func TestRenderFromStream_Tagged(t *testing.T) {
	args, msg := parsePipeArgs([]string{"-x", ".ts", "-y", ".mem_pct", "--tag", ".coordinator_id", "--bucket", "1m", "-a", "avg"})
	if msg != "" {
		t.Fatalf("parse: %s", msg)
	}
	lines := []string{
		`{"ts":"2026-01-01T00:00:10Z","coordinator_id":"i-aaa","mem_pct":40}`,
		`{"ts":"2026-01-01T00:01:10Z","coordinator_id":"i-aaa","mem_pct":44}`,
		`{"ts":"2026-01-01T00:02:10Z","coordinator_id":"i-aaa","mem_pct":48}`,
		`{"ts":"2026-01-01T00:00:10Z","coordinator_id":"i-bbb","mem_pct":60}`,
		`{"ts":"2026-01-01T00:01:10Z","coordinator_id":"i-bbb","mem_pct":64}`,
		`{"ts":"2026-01-01T00:02:10Z","coordinator_id":"i-bbb","mem_pct":68}`,
	}
	out, errMsg := RenderFromStream(lines, args, 80, 20)
	if errMsg != "" {
		t.Fatalf("render: %s", errMsg)
	}
	for _, want := range []string{"i-aaa", "i-bbb"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing tag label %q in rendered output", want)
		}
	}
}

func TestRenderFromStream_TaggedNoData(t *testing.T) {
	args, _ := parsePipeArgs([]string{"-x", ".ts", "-y", ".v", "--tag", ".host"})
	_, errMsg := RenderFromStream([]string{`{"ts":"2026-01-01T00:00:00Z","v":1}`}, args, 80, 20)
	if errMsg == "" {
		t.Fatal("expected 'no data points' error for rows with no host field")
	}
}

func TestParseTime(t *testing.T) {
	if _, ok := parseTime("2026-01-01T00:00:00Z", ""); !ok {
		t.Error("RFC3339 not parsed")
	}
	if _, ok := parseTime("1736000000", ""); !ok {
		t.Error("epoch seconds not parsed")
	}
	if _, ok := parseTime("1736000000.5", ""); !ok {
		t.Error("epoch float not parsed")
	}
	if _, ok := parseTime("not a time", ""); ok {
		t.Error("string parsed as time unexpectedly")
	}
}

// `$ts` prints the row's zone-less UTC timestamp the way DuckDB casts it,
// with the microseconds only when there are any, so `jq '{ts: $ts, ...}' |
// chart -x .ts` has to read both forms without a ::time() format.
func TestParseTime_DuckDBStamp(t *testing.T) {
	for in, want := range map[string]time.Time{
		"2026-10-18 12:00:00":        time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
		"2026-10-18 12:00:00.123456": time.Date(2026, 10, 18, 12, 0, 0, 123456000, time.UTC),
	} {
		got, ok := parseTime(in, "")
		if !ok || !got.Equal(want) {
			t.Errorf("parseTime(%q) = %v, %v; want %v", in, got, ok, want)
		}
	}
}

// TestRenderFromStream_DuckDBStampX: `jq -c '{ts: $ts, pct: ...}' | chart -x .ts`
// is the way to plot a metrics file now that its JSON carries no ts of its
// own, so the stamp `$ts` prints has to work as the x axis, including rows
// that differ in whether they have microseconds.
func TestRenderFromStream_DuckDBStampX(t *testing.T) {
	args, errMsg := parsePipeArgs([]string{"-x", ".ts", "-y", ".pct", "--bucket", "1m", "-a", "avg"})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	lines := []string{
		`{"ts":"2026-10-18 12:00:00","pct":10}`,
		`{"ts":"2026-10-18 12:00:30.250000","pct":20}`,
		`{"ts":"2026-10-18 12:01:15.5","pct":30}`,
	}
	if _, errMsg := RenderFromStream(lines, args, 80, 20); errMsg != "" {
		t.Fatalf("RenderFromStream: %s", errMsg)
	}
}

// OB-5: an onboarding lesson once charted `tail main.log | grep ERROR | chart -x 1
// --bucket 5m`. tail returns ~10 rows, which can all land in one bucket. This
// pins what the native renderer does with that shape, so a "nothing to plot"
// theory can be checked without a browser.
func TestRenderFromStream_ErrorRowsInOneBucket(t *testing.T) {
	args, msg := parsePipeArgs([]string{"-x", "1", "--bucket", "5m"})
	if msg != "" {
		t.Fatal(msg)
	}
	oneBucket := []string{
		"2026-10-10T12:00:01Z ERROR a",
		"2026-10-10T12:00:20Z ERROR b",
		"2026-10-10T12:01:05Z ERROR c",
		"2026-10-10T12:02:40Z ERROR d",
	}
	out, errMsg := RenderFromStream(oneBucket, args, 80, 12)
	t.Logf("one bucket: errMsg=%q\n%s", errMsg, out)
	if errMsg != "" || out == "" {
		t.Errorf("single-bucket input should still render something, got err=%q out=%q", errMsg, out)
	}

	threeBuckets := append(oneBucket,
		"2026-10-10T12:07:00Z ERROR e", "2026-10-10T12:08:00Z ERROR f", "2026-10-10T12:12:00Z ERROR g")
	out, errMsg = RenderFromStream(threeBuckets, args, 80, 12)
	t.Logf("three buckets: errMsg=%q\n%s", errMsg, out)
	if errMsg != "" {
		t.Errorf("multi-bucket input: %s", errMsg)
	}
}
