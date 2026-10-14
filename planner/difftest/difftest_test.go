// Package difftest is the planner's differential harness (see
// docs/plans/planner-ir.md, "Differential harness"). Every pipeline runs
// twice over the same generated rows: once as Plan() compiles it, through
// the real compute engine, and once as the reference, a scan of the
// source with every stage in compute. The outputs must match byte for
// byte. A pushdown rule is trusted because this passes, not because a
// recognizer has its own parity test.
package difftest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/compute/computetest"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// TestDifferential runs the corpus against generated rows on two shards.
func TestDifferential(t *testing.T) {
	h := newHarness(t, generateRows(400), 2)
	for _, p := range corpus(t) {
		t.Run(p, func(t *testing.T) { h.check(t, p) })
	}
}

// TestRowStagesAreRowwise checks the planner's row-stage classifier:
// a text filter it accepts must keep, run one row at a time, exactly
// the lines it keeps run over the whole stream. (jq select chains read
// the payload and head/tail count, so they're checked by
// TestDifferential instead.)
func TestRowStagesAreRowwise(t *testing.T) {
	rows := generateRows(400)
	var input strings.Builder
	for _, r := range rows {
		input.WriteString(r.Raw + "\n")
	}
	checked := map[string]bool{}
	for _, p := range corpus(t) {
		if _, err := bash.Parse(p); err != nil {
			continue // a planner test's deliberately malformed input
		}
		_, stages := splitSource(t, p)
		for _, st := range stages {
			cmd := strings.Fields(st)[0]
			if checked[st] || !planner.IsRowStage(st) || (cmd != "grep" && cmd != "sed" && cmd != "awk") || readsMetaVars([]string{st}) {
				continue
			}
			checked[st] = true
			var stream, stderr bytes.Buffer
			if _, err := bash.RunStreaming(st, strings.NewReader(input.String()), &stream, &stderr); err != nil {
				t.Fatalf("%s: %v", st, err)
			}
			rs, err := compute.NewRowStages([]string{st})
			if err != nil {
				t.Fatal(err)
			}
			var rowwise strings.Builder
			for _, r := range rows {
				if rs.Keep(r.Raw, "", nil) {
					rowwise.WriteString(r.Raw + "\n")
				}
			}
			if stream.String() != rowwise.String() {
				t.Errorf("%s keeps different lines per row than over the stream: %s", st, firstDiff(rowwise.String(), stream.String()))
			}
		}
	}
	if len(checked) == 0 {
		t.Fatal("no row stages in the corpus")
	}
}

type harness struct {
	engine *compute.Engine
	shards []*shard.DuckDBShard
}

func newHarness(t *testing.T, rows []*pb.LogRow, nShards int) *harness {
	t.Helper()
	h := &harness{engine: &compute.Engine{}}
	for i := 0; i < nShards; i++ {
		sh, err := shard.NewDuckDBShard(fmt.Sprintf("shard-%d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sh.Close() })
		h.engine.RegisterResolver(computetest.NewInProcessResolver(sh.ID(), sh))
		h.shards = append(h.shards, sh)
	}
	// Round-robin, so every shard holds a slice of each source.
	for i, sh := range h.shards {
		var part []*pb.LogRow
		for j := i; j < len(rows); j += nShards {
			part = append(part, rows[j])
		}
		if err := sh.Ingest(part, nil); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *harness) check(t *testing.T, pipeline string) {
	plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Skipf("not plannable: %v", err)
	}
	source, stages := splitSource(t, pipeline)
	got, gotErr := h.runPlan(plan)
	want, wantErr := h.runReference(source, stages, readsMetaVars(stages))
	switch {
	case gotErr != nil && wantErr != nil:
		// Both fail, e.g. a stage warp doesn't support. Same behavior.
		return
	case gotErr != nil:
		t.Fatalf("plan fails, reference doesn't: %v\n%s", gotErr, planner.Explain(pipeline))
	case wantErr != nil:
		t.Fatalf("reference fails, plan doesn't: %v", wantErr)
	}
	// With no merge key, rows reach compute in shard arrival order, so
	// anything order-sensitive (ties in a sort -k, say) is legitimately
	// nondeterministic; compare the lines as a multiset.
	if plan.ShardOrderBy == "" {
		got, want = sortedLines(got), sortedLines(want)
	}
	if got != want {
		t.Errorf("plan and reference differ: %s\n%s", firstDiff(got, want), planner.Explain(pipeline))
	}
}

func sortedLines(s string) string {
	lines := strings.Split(s, "\n")
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// firstDiff describes the first line where plan and reference differ.
func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) || i < len(w); i++ {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl {
			return fmt.Sprintf("line %d (plan %d lines, reference %d)\n  plan:      %q\n  reference: %q", i+1, len(g), len(w), gl, wl)
		}
	}
	return ""
}

// runPlan dispatches the plan the way the coordinator does.
func (h *harness) runPlan(plan *planner.QueryPlan) (string, error) {
	return h.dispatch(&pb.ExecuteQueryRequest{
		Sql:               plan.DirectShardSQL,
		Args:              argStrings(plan.Args),
		Stages:            plan.ComputeStages,
		RowStages:         plan.RowStages,
		StagesReadPayload: plan.StagesReadPayload,
		MergeKey:          plan.ShardOrderBy,
		Limit:             int32(plan.Limit),
		KeepLast:          plan.TailMode,
	})
}

// dispatch runs a request over every shard through the compute engine.
func (h *harness) dispatch(req *pb.ExecuteQueryRequest) (string, error) {
	// A shard that fails its SQL just ends its stream, so check the SQL
	// first or an error would look like an empty result.
	for _, sh := range h.shards {
		if _, err := sh.Query(req.Sql, toAny(req.Args)...); err != nil {
			return "", fmt.Errorf("shard SQL %s: %w", req.Sql, err)
		}
		req.Shards = append(req.Shards, &pb.ShardDescriptor{ShardId: sh.ID()})
	}
	stream, err := h.engine.ExecuteQuery(context.Background(), req)
	if err != nil {
		return "", err
	}
	return drain(stream)
}

// runReference is the trivially correct plan: scan the source (its
// head/tail count included), then run every stage in compute. Leading
// stages that pass rows through run per row, so the stage after them
// reads the row (a jq reads its payload); weft defines that, and the
// plan must agree whether or not those stages pushed down.
//
// Meta-variables reach jq by being spliced into the JSON it reads, so
// projecting them would also show them to `keys` or `.`; the reference
// projects them only when a stage names one, as compute does.
func (h *harness) runReference(source string, stages []string, meta bool) (string, error) {
	src, err := planner.Plan(source, 0, "", time.Time{}, time.Time{})
	if err != nil {
		return "", err
	}
	cols := ""
	if meta {
		cols = strings.Join(planner.MetaVarColumns, ", ") + ", "
	}
	sql := strings.Replace(src.DirectShardSQL, "SELECT raw, ts FROM logs",
		"SELECT raw, ts, "+cols+"CAST(payload AS VARCHAR) AS "+planner.PayloadColumn+" FROM logs", 1)
	sql = strings.Replace(sql, "SELECT raw, ts FROM (",
		"SELECT raw, ts, "+cols+planner.PayloadColumn+" FROM (", 1)
	n := 0
	for n < len(stages) && planner.IsRowStage(stages[n]) {
		n++
	}
	rest := stages[n:]
	return h.dispatch(&pb.ExecuteQueryRequest{
		Sql:               sql,
		Args:              argStrings(src.Args),
		Stages:            rest,
		RowStages:         stages[:n],
		StagesReadPayload: len(rest) > 0 && strings.HasPrefix(rest[0], "jq "),
		MergeKey:          src.ShardOrderBy,
		Limit:             int32(src.Limit),
		KeepLast:          src.TailMode,
	})
}

// readsMetaVars reports whether any stage names a meta-variable.
func readsMetaVars(stages []string) bool {
	for _, st := range stages {
		for name := range planner.JQMetaVars {
			if strings.Contains(st, "."+name) {
				return true
			}
		}
		for name := range planner.AwkMetaVars {
			if regexp.MustCompile(`\b` + name + `\b`).MatchString(st) {
				return true
			}
		}
	}
	return false
}

func splitSource(t *testing.T, pipeline string) (string, []string) {
	script, err := bash.Parse(pipeline)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, c := range script.Commands[0].First.Commands {
		parts = append(parts, bash.Unparse(&bash.Script{Commands: []*bash.List{{First: &bash.Pipeline{Commands: []bash.Command{c}}}}}))
	}
	return parts[0], parts[1:]
}

func drain(s compute.LineStream) (string, error) {
	var lines []string
	for {
		l, ok, err := s.Next(context.Background())
		if err != nil {
			return "", err
		}
		if !ok {
			return strings.Join(lines, "\n"), nil
		}
		lines = append(lines, l.Raw)
	}
}

func argStrings(args []any) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = fmt.Sprint(a)
	}
	return out
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// corpus is every pipeline in the planner's own tests whose source is a
// finite cat/head/tail, plus the pipelines below that exercise rules the
// tests don't. Follow mode, redirects, alerts and traces don't have a
// finite batch to compare.
func corpus(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	lit := regexp.MustCompile("`((?:cat|head|tail) [^`]*)`")
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if seen[p] || strings.Contains(p, " -f ") || strings.Contains(p, ">") ||
			strings.Contains(p, "alert") || strings.Contains(p, "trace") {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range lit.FindAllStringSubmatch(string(src), -1) {
			add(m[1])
		}
	}
	for _, p := range extraCorpus {
		add(p)
	}
	sort.Strings(out)
	return out
}

var extraCorpus = []string{
	`cat /log/acme/api/main.log | jq 'select(.status == 500)'`,
	`cat /log/acme/api/main.log | jq 'select(.status == 500) | .action'`,
	`cat /log/acme/api/main.log | jq -r 'select(.status == 500) | .action'`,
	`cat /log/acme/api/main.log | jq '{a: .action, s: .status}'`,
	`cat /log/acme/api/main.log | jq '[.status, .action]'`,
	`cat /log/acme/api/main.log | jq '.foo.bar'`,
	`cat /log/acme/api/main.log | jq '._level'`,
	`cat /log/acme/api/main.log | jq 'select(._level == "ERROR") | {l: ._level, h: ._host}'`,
	`tail -n 30 /log/acme/api/main.log | jq 'select(.status == 500)' | wc -l`,
	`head -n 30 /log/acme/api/main.log | jq 'select(.status == 500)'`,
	`cat /log/acme/api/main.log | jq 'select(.status == 500)' | grep login`,
	`cat /log/acme/api/main.log | jq 'select(has("items")) | .items'`,
	`cat /log/acme/api/main.log | awk 'LEVEL == "ERROR" { print "E" } LEVEL == "WARN" { print "W" }'`,
	`cat /log/acme/api/main.log | awk 'LEVEL != "INFO"'`,
	`cat /log/acme/api/main.log | awk 'LEVEL < "WARN"'`,
	`cat /log/acme/api/main.log | awk 'LEVEL == ""'`,
	`cat /log/acme/api/main.log | awk 'DURATION_NS > 1000'`,
	`cat /log/acme/api/main.log | awk 'DURATION_NS == 0'`,
	`cat /log/acme/api/main.log | awk '{ print LEVEL, HOST }'`,
	`cat /log/acme/api/main.log | awk '{ print LEVEL "/" 1.5 }'`,
	`cat /log/acme/api/main.log | awk 'LEVEL == "ERROR" && $2 == "x"'`,
	`cat /log/acme/api/main.log | awk '/timeout/ { n++ } END { print n }'`,
	`cat /log/acme/api/main.log | awk 'NR >= 3 && NR <= 7'`,
	`cat /log/acme/api/main.log | awk 'LEVEL == "ERROR" { print NR }'`,
	`cat /log/acme/api/main.log | awk '{ print "it'\''s" }'`,
	`tail -n 40 /log/acme/api/main.log | awk 'LEVEL == "ERROR"' | wc -l`,
	`cat /log/acme/api/main.log | grep -v timeout | sort | uniq -c`,
	`cat /log/acme/api/main.log | sed -n '/login/p'`,
}

// generateRows builds a deterministic mix of the line shapes weft
// ingests, across the sources the corpus reads. Timestamps are unique,
// so merge order across shards is deterministic and outputs compare
// exactly.
func generateRows(n int) []*pb.LogRow {
	r := rand.New(rand.NewSource(1))
	pick := func(xs ...string) string { return xs[r.Intn(len(xs))] }
	sources := []struct{ ns, svc, kind string }{
		{"vimgym", "", "main"}, {"callbox", "", "main"}, {"acme", "api", "main"}, {"acme", "web", "main"},
	}
	base := time.Date(2026, 10, 13, 23, 50, 0, 0, time.UTC) // spans midnight
	var rows []*pb.LogRow
	for i := 0; i < n; i++ {
		src := sources[r.Intn(len(sources))]
		level := pick("ERROR", "WARN", "INFO", "DEBUG", "")
		ts := base.Add(time.Duration(i) * 7 * time.Second)
		if i%3 == 0 {
			ts = ts.Add(time.Duration(r.Intn(1000)) * time.Millisecond)
		}
		payload := map[string]any{}
		if r.Intn(4) > 0 {
			switch r.Intn(3) {
			case 0:
				payload["status"] = 500
			case 1:
				payload["status"] = 200
			default:
				payload["status"] = "500"
			}
		}
		if r.Intn(3) > 0 {
			payload["action"] = pick("login", "search", "it's", "naïve café", `quote"d`, "")
		}
		if r.Intn(4) == 0 {
			payload["foo"] = map[string]any{"bar": r.Intn(3)}
		}
		if r.Intn(5) == 0 {
			payload["items"] = []any{1, "two", 3.5}
		}
		if r.Intn(5) == 0 {
			payload["duration_ms"] = float64(r.Intn(3000)) / 2
		}
		if r.Intn(6) == 0 {
			payload["success"] = r.Intn(2) == 0
		}
		if r.Intn(6) == 0 {
			payload["nothing"] = nil
		}
		body, _ := json.Marshal(payload)
		msg := pick("connection timeout", "login ok", "user it's here", "cache warmed 2500 keys", "  padded   text  ")
		var raw string
		payloadJSON := body
		switch r.Intn(5) {
		case 0: // JSON line
			raw = string(body)
		case 1: // prefixed text with a JSON tail
			raw = fmt.Sprintf("%s %s %s %s", ts.Format(time.RFC3339), level, msg, body)
		case 2: // kevcons envelope, plain text
			raw = fmt.Sprintf("[%s] [i-%03d] %s %s", ts.Format("2006-01-02 15:04:05"), r.Intn(5), level, msg)
			payloadJSON = []byte("{}")
		case 3: // l2met
			raw = fmt.Sprintf("source=web.%d sample#memory_total=%d.00MB", r.Intn(3), 100+r.Intn(900))
		default: // plain text
			raw = msg
			payloadJSON = []byte("{}")
		}
		raw = fmt.Sprintf("%s #%d", raw, i) // unique, so ORDER BY raw has no ties
		if strings.HasPrefix(raw, "{") {
			raw = string(body) // a JSON line stays valid JSON
		}
		var dur int64
		if r.Intn(3) == 0 {
			dur = int64(r.Intn(5000))
		}
		rows = append(rows, &pb.LogRow{
			Raw:         raw,
			Fields:      strings.Fields(raw),
			TsUnixNs:    ts.UnixNano(),
			Level:       level,
			Namespace:   src.ns,
			Service:     src.svc,
			Kind:        src.kind,
			Instance:    fmt.Sprintf("i-%03d", r.Intn(5)),
			CustomerId:  1,
			PayloadJson: payloadJSON,
			DurationNs:  dur,
		})
	}
	return rows
}
