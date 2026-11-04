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
	"sync/atomic"
	"testing"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/compute/computetest"
	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// TestDifferential runs the corpus against generated rows on two shards.
// Every aggregate the planner computes in the shard must appear in it.
func TestDifferential(t *testing.T) {
	h := newHarness(t, generateRows(400), 2)
	renders := map[planner.GroupRender]int{}
	var ordered, limited int
	for _, p := range corpus(t) {
		t.Run(p, func(t *testing.T) {
			if plan := h.check(t, p); plan != nil && plan.Group != nil {
				renders[plan.Group.Render]++
				if plan.Group.ByCount {
					ordered++
				}
				if plan.Group.Limit > 0 {
					limited++
				}
			}
		})
	}
	for _, r := range []planner.GroupRender{planner.RenderTotal, planner.RenderLine, planner.RenderCountLine, planner.RenderDistinctTotal, planner.RenderKeyCount, planner.RenderBytes, planner.RenderNumericMax, planner.RenderNumericMin} {
		if renders[r] == 0 {
			t.Errorf("no pipeline in the corpus groups as %s in the shard", r)
		}
	}
	if ordered == 0 || limited == 0 {
		t.Errorf("the corpus orders %d and limits %d groups by count, want both", ordered, limited)
	}
}

// TestDifferentialLineSplits checks the stages that read lines, not
// rows, over rows holding several lines or nothing: an aggregate counts
// what compute would have fed the stage, and a hoisted filter keeps the
// lines grep would. (A trailing carriage return
// is docs/known-issues.md's.)
func TestDifferentialLineSplits(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i, raw := range []string{
		"a", "b\nc", "", "c\n", "b", "\n", "a", "multi\nline\nrow", "b", "line\n\n",
	} {
		rows = append(rows, &pb.LogRow{
			Raw: raw, TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1, PayloadJson: []byte("{}"),
		})
	}
	h := newHarness(t, rows, 2)
	for _, p := range []string{
		`cat /log/acme/api/main.log | wc -l`,
		`cat /log/acme/api/main.log | sort | uniq -c`,
		`cat /log/acme/api/main.log | sort -r | uniq`,
		`cat /log/acme/api/main.log | sort -u`,
		`cat /log/acme/api/main.log | grep nothing-matches | wc -l`,
		`cat /log/acme/api/main.log | grep -c nothing-matches`,
		`cat /log/acme/api/main.log | grep -c b`,
		`cat /log/acme/api/main.log | grep -vc b`,
		`cat /log/acme/api/main.log | grep -c '^$'`,
		`cat /log/acme/api/main.log | grep -ci LINE`,
		`cat /log/acme/api/main.log | grep nothing-matches | sort | uniq -c`,
		`cat /log/acme/api/main.log | sort | grep -v b | uniq -c`,
	} {
		t.Run(p, func(t *testing.T) {
			if plan := h.check(t, p); plan != nil && plan.Group == nil {
				t.Errorf("didn't aggregate in the shard:\n%s", planner.Explain(p))
			}
		})
	}
	for _, p := range []string{
		`cat /log/acme/api/main.log | sort | grep b`,
		`cat /log/acme/api/main.log | sort -r | grep -v b`,
		`cat /log/acme/api/main.log | sort | grep '^$'`,
		`cat /log/acme/api/main.log | grep line | sort -u | sed -n '/^l/p'`,
		`cat /log/acme/api/main.log | cut -d x -f 1 | grep -v b`,
	} {
		t.Run(p, func(t *testing.T) {
			plan := h.check(t, p)
			if plan == nil {
				return
			}
			for _, s := range plan.ComputeStages {
				if strings.HasPrefix(s, "grep") || strings.HasPrefix(s, "sed") {
					t.Errorf("%q runs in compute, want it hoisted:\n%s", s, planner.Explain(p))
				}
			}
		})
	}
}

// TestDifferentialLineShapes checks $NF, length and grep -E over the lines
// they are easiest to get wrong on: blank, only blanks, one field, runs of
// blanks and tabs, and text whose bytes outnumber its characters. Each row is
// one line, so the stage reads what the shard's row holds.
func TestDifferentialLineShapes(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i, raw := range []string{
		"", "   ", "\t", "a", "a b", "a  b   c", " lead trail ", "x\ty", "naïve café", "日本語 テスト",
		"é", "ß", "12 5", "7", "ERROR boom", "warn 12",
	} {
		rows = append(rows, &pb.LogRow{
			Raw: raw, TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1, PayloadJson: []byte("{}"),
		})
	}
	h := newHarness(t, rows, 2)
	for _, p := range []string{
		`cat /log/acme/api/main.log | awk '{ print $NF }'`,
		`cat /log/acme/api/main.log | awk '{ print $1, $NF }'`,
		`cat /log/acme/api/main.log | awk '$NF == "b"'`,
		`cat /log/acme/api/main.log | awk '$NF == ""'`,
		`cat /log/acme/api/main.log | awk '$NF ~ /^[a-z]/'`,
		`cat /log/acme/api/main.log | awk '{ print $NF }' | sort | uniq -c`,
		`cat /log/acme/api/main.log | awk '{ c[$NF]++ } END { for (k in c) print k, c[k] }'`,
		`cat /log/acme/api/main.log | awk '{ print length }'`,
		`cat /log/acme/api/main.log | awk '{ print length($0), length($1), length($NF) }'`,
		`cat /log/acme/api/main.log | awk 'length > 3'`,
		`cat /log/acme/api/main.log | awk 'length($0) == 0'`,
		`cat /log/acme/api/main.log | awk 'length($2) == 0'`,
		`cat /log/acme/api/main.log | awk 'length($NF) >= 2 { print $0 }'`,
		`cat /log/acme/api/main.log | awk '{ print length }' | sort | uniq -c`,
		`cat /log/acme/api/main.log | grep -E 'a|b'`,
		`cat /log/acme/api/main.log | grep -Ei 'ERROR|WARN'`,
		`cat /log/acme/api/main.log | grep -Ei 'É'`,
		`cat /log/acme/api/main.log | grep -Ev '^$|^ '`,
		`cat /log/acme/api/main.log | grep -E '^\s*$'`,
		`cat /log/acme/api/main.log | grep -E '[0-9]+$' | wc -l`,
	} {
		t.Run(p, func(t *testing.T) {
			if plan := h.check(t, p); plan != nil && len(plan.ComputeStages) != 0 {
				t.Errorf("stages left in compute %q, want them lowered:\n%s", plan.ComputeStages, planner.Explain(p))
			}
		})
	}
	// A comparison with a number is a prefilter: awk decides number or text
	// per value, so the stage still runs.
	h.check(t, `cat /log/acme/api/main.log | awk '$NF > 6'`)
	// Stages that stay in compute are cut down to the fields they read: the
	// same $N, $NF and NF over lines of no fields, one, and many.
	for _, p := range []string{
		`cat /log/acme/api/main.log | awk '{ n++ } END { print n }'`,
		`cat /log/acme/api/main.log | awk '{ printf "[%s][%s][%d]\n", $1, $NF, NF }'`,
		`cat /log/acme/api/main.log | awk '{ s = s "," $2 } END { print s }'`,
		`cat /log/acme/api/main.log | awk '{ if (length($1) > m) m = length($1) } END { print m }'`,
		`cat /log/acme/api/main.log | awk '{ printf "%s|%s|%s\n", $3, $2, NF }'`,
		`cat /log/acme/api/main.log | awk '{ n += NF } END { print n }'`,
		`cat /log/acme/api/main.log | awk '{ a[$NF] = a[$NF] + 1 } END { for (k in a) print k "=" a[k] }'`,
	} {
		t.Run("projected "+p, func(t *testing.T) {
			plan := h.check(t, p)
			if plan != nil && (len(plan.ComputeStages) != 1 || plan.FieldSQL == "" || plan.FieldSpec == nil) {
				t.Errorf("not cut down to its fields:\n%s", planner.Explain(p))
			}
		})
	}
}

// TestDifferentialFieldProjectionLines: an event of several lines is stored a
// row per line (ingest.SplitLines), and a stage cut down to its fields reads
// each line's own: $1 is its first field, NF its count. The stage gets the same
// answer from whole lines and from the field form.
func TestDifferentialFieldProjectionLines(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i, raw := range []string{
		"a 1 x", "b 2\nc 3 y z", "", "d\n", "e 5\n\nf 6 w", "  g   7  ", "h\ti\tj", "k 9\r\nl 10",
	} {
		rows = append(rows, &pb.LogRow{
			Raw: raw, TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1,
		})
	}
	h := newHarness(t, ingest.SplitLines(rows), 2)
	for _, p := range []string{
		`cat /log/acme/api/main.log | awk '{ n++ } END { print n }'`,
		`cat /log/acme/api/main.log | awk '{ s += $2 } END { print s }'`,
		`cat /log/acme/api/main.log | awk '{ printf "%s;%s;%d\n", $1, $NF, NF }'`,
		`cat /log/acme/api/main.log | awk '{ c[NF]++ } END { for (k in c) print k, c[k] }'`,
		`cat /log/acme/api/main.log | awk '{ l = l $1 } END { print l }'`,
		`cat /log/acme/api/main.log | awk 'NR % 2 == 0 { printf "%d:%s\n", NR, $1 }'`,
	} {
		t.Run(p, func(t *testing.T) {
			plan := h.check(t, p)
			if plan == nil || plan.FieldSQL == "" {
				t.Fatalf("no field form:\n%s", planner.Explain(p))
			}
			source, stages := splitSource(t, p)
			want, wantErr := h.runReference(source, stages, readsMetaVars(stages))
			got, gotErr := h.runFieldForm(plan)
			if wantErr != nil || gotErr != nil {
				t.Fatalf("errors: field form %v, reference %v", gotErr, wantErr)
			}
			if plan.ShardOrderBy == "" {
				got, want = sortedLines(got), sortedLines(want)
			}
			if got != want {
				t.Errorf("field form differs from the reference: %s", firstDiff(got, want))
			}
		})
	}
}

// TestFieldProjectionShrinksTheWire: the point of the cut. Over long lines of
// many fields, what the shards return in the field form for a stage reading
// two of them is a fraction of what they return for the whole lines, and the
// same number of lines.
func TestFieldProjectionShrinksTheWire(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		raw := fmt.Sprintf("2026-10-13T12:00:%02d INFO req-%d host-%d GET /api/v1/things/%d status=200 bytes=%d user=u%d agent=Mozilla/5.0 trace=%032d",
			i%60, i, i%7, i, 1000+i, i%13, i)
		rows = append(rows, &pb.LogRow{
			Raw: raw, TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1, PayloadJson: []byte("{}"),
		})
	}
	h := newHarness(t, rows, 2)
	const whole = `cat /log/acme/api/main.log`
	const cut = whole + ` | awk '{ s[$4] += $8 } END { for (k in s) print k, s[k] }'`
	wire := func(p string) (lines, bytes int) {
		plan, err := planner.Plan(p, 0, "", time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		sql := plan.DirectShardSQL
		if plan.FieldSQL != "" {
			sql = plan.FieldSQL
		}
		for _, sh := range h.shards {
			got, err := sh.Query(context.Background(), sql, toAny(argStrings(plan.Args))...)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range got {
				lines++
				bytes += len(r.Raw)
				for _, f := range r.Fields {
					bytes += len(f)
				}
			}
		}
		return lines, bytes
	}
	wholeLines, wholeBytes := wire(whole)
	cutLines, cutBytes := wire(cut)
	if cutLines != wholeLines || wholeLines != 200 {
		t.Errorf("lines: whole %d, cut %d, want 200 each", wholeLines, cutLines)
	}
	if cutBytes*4 > wholeBytes {
		t.Errorf("the cut sends %d bytes of %d: not under a quarter", cutBytes, wholeBytes)
	}
	t.Logf("whole lines %d bytes, cut %d bytes (%.1f%%)", wholeBytes, cutBytes, 100*float64(cutBytes)/float64(wholeBytes))
	h.check(t, cut)
}

// TestDifferentialJqPayloadKeys: a jq stage that reads only some top-level
// keys is sent an object of just those. It must read the same values, down to
// how a number is written, and anything that looks at the whole object must
// still see all of it. The payloads are the awkward ones: odd numbers,
// escapes, nulls, duplicate keys, and payloads that aren't objects.
func TestDifferentialJqPayloadKeys(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i, payload := range []string{
		`{"a":1.50,"b":1e3,"c":-0,"d":12345678901234567890,"e":"é\u00e9\n\t\"q\"","f":[1,{"g":null}],"h":null}`,
		`{}`,
		`{"a":{"b":{"c":1}},"items":[3,1,2],"tags":["x","y"],"status":500,"action":"login"}`,
		`{"status":"500","action":"Logout","items":[],"tags":[]}`,
		`{"status":200,"action":"naïve café","foo":{"bar":"baz"},"duration_ms":12.5}`,
		`{"a":1,"a":2,"status":404}`,
		`[1,2,3]`,
		`"just a string"`,
		`3`,
		`null`,
		`{"a":true,"b":false,"c":0.1,"d":1e-7,"e":123456789012,"f":0.30000000000000004}`,
		`{"status":500,"action":"login","extra":{"deep":[{"k":1},{"k":2}]},"items":[1,2,3]}`,
		`{"a":"web-0","b":"x: -0, y","c":[-0, 1],"d":{"e":-0},"status":201}`,
		`{"a":"kafka-0","b":-0.0,"c":-0e5,"d":[ -0 ],"action":"-0"}`,
	} {
		rows = append(rows, &pb.LogRow{
			Raw: fmt.Sprintf("row %d", i), TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1, PayloadJson: []byte(payload),
		})
	}
	h := newHarness(t, rows, 2)
	projected := []string{
		`jq '.a'`, `jq '.b, .c, .d'`, `jq -r '.e'`, `jq '.f'`, `jq '.h'`, `jq '.a.b.c'`,
		`jq '.foo | length'`, `jq '.items[]'`, `jq '.tags | tostring'`, `jq '.items | add'`,
		`jq '.items | map(. * 2)'`, `jq '[.status, .action] | join(",")'`,
		`jq '{s: .status, a: .action}'`, `jq '{status, action}'`, `jq '[.a, .b, .c]'`,
		`jq '.duration_ms * 2'`, `jq '"\(.action)-\(.status)"'`, `jq '.foo.bar // "none"'`,
		`jq 'if .status == 500 then .action else empty end'`, `jq '.action | ascii_downcase'`,
		`jq '.status | tostring'`, `jq 'select(.action | test("log")) | .status'`,
		`jq 'select(.status == 500) | .extra.deep[0]'`, `jq '(.a // 0) + (.b // 0)'`,
		`jq '.missing'`, `jq '.missing.deeper'`, `jq '.a == null'`, `jq '[.items[] | select(. > 1)]'`,
		`jq -c '{a, d, e}'`, `jq '.c, .d, .a | tostring'`,
	}
	whole := []string{
		`jq '.'`, `jq 'keys'`, `jq 'length'`, `jq 'to_entries'`, `jq 'has("a")'`, `jq 'del(.a)'`,
		`jq '. + {x: 1}'`, `jq '[.[]]'`, `jq '..'`, `jq 'with_entries(.)'`, `jq 'select(.status == 500) | .'`,
		`jq '.a, .'`, `jq '[., .a]'`, `jq 'map_values(.)'`, `jq '. as $o | $o.a'`, `jq 'tojson'`,
		`jq '{a: .a} + .'`, `jq 'if .status then . else empty end'`, `jq -s 'length'`,
	}
	// A program the planner lowers whole has no compute stage and nothing to
	// cut; only the stages that stay in compute are this test's.
	for _, p := range projected {
		p := `cat /log/acme/api/main.log | ` + p
		t.Run(p, func(t *testing.T) {
			plan, err := planner.Plan(p, 0, "", time.Time{}, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.ComputeStages) == 0 {
				t.Skip("lowered whole")
			}
			if !strings.Contains(plan.DirectShardSQL, "json_object(") {
				t.Errorf("not cut down to its keys:\n%s", planner.Explain(p))
			}
			h.check(t, p)
		})
	}
	for _, p := range whole {
		p := `cat /log/acme/api/main.log | ` + p
		t.Run(p, func(t *testing.T) {
			plan := h.check(t, p)
			if plan != nil && strings.Contains(plan.DirectShardSQL, "json_object(") {
				t.Errorf("cut down, but it reads the whole object:\n%s", planner.Explain(p))
			}
		})
	}
}

// TestJqProjectionShrinksTheWire: a jq stage reading two keys of a wide
// payload is sent a fraction of the payload, for the same rows.
func TestJqProjectionShrinksTheWire(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 100; i++ {
		payload := map[string]any{"status": 200 + i%5, "action": fmt.Sprintf("act-%d", i%4)}
		for k := 0; k < 30; k++ {
			payload[fmt.Sprintf("field_%02d", k)] = fmt.Sprintf("a longish value for field %d of row %d, to take up room", k, i)
		}
		body, _ := json.Marshal(payload)
		rows = append(rows, &pb.LogRow{
			Raw: fmt.Sprintf("row %d", i), TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1, PayloadJson: body,
		})
	}
	h := newHarness(t, rows, 2)
	const whole = `cat /log/acme/api/main.log | jq 'keys'`
	const cut = `cat /log/acme/api/main.log | jq '[.status, .action] | join("/")'`
	wire := func(p string) (n, bytes int) {
		plan, err := planner.Plan(p, 0, "", time.Time{}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		for _, sh := range h.shards {
			got, err := sh.Query(context.Background(), plan.DirectShardSQL, toAny(argStrings(plan.Args))...)
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range got {
				n++
				bytes += len(r.Raw)
			}
		}
		return n, bytes
	}
	wholeRows, wholeBytes := wire(whole)
	cutRows, cutBytes := wire(cut)
	if cutRows != wholeRows || wholeRows != 100 {
		t.Errorf("rows: whole %d, cut %d, want 100 each", wholeRows, cutRows)
	}
	if cutBytes*5 > wholeBytes {
		t.Errorf("the cut sends %d bytes of %d: not under a fifth", cutBytes, wholeBytes)
	}
	t.Logf("whole payloads %d bytes, cut %d bytes (%.1f%%)", wholeBytes, cutBytes, 100*float64(cutBytes)/float64(wholeBytes))
	h.check(t, cut)
}

// TestDifferentialEventMetaVars checks EVENT_ID and LINE_NO, which
// one-line events leave unset, over events of several lines.
func TestDifferentialEventMetaVars(t *testing.T) {
	var rows []*pb.LogRow
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for i, raw := range []string{
		"a", "b\nc\nd", "e", "f\ng", "h", "i\nj\nk\nl", "m",
	} {
		rows = append(rows, &pb.LogRow{
			Raw: raw, TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
			Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1,
		})
	}
	h := newHarness(t, ingest.SplitLines(rows), 2)
	for _, p := range []string{
		`cat /log/acme/api/main.log | awk 'LINE_NO == 0'`,
		`cat /log/acme/api/main.log | awk 'LINE_NO > 0'`,
		`cat /log/acme/api/main.log | awk 'LINE_NO >= 2'`,
		`cat /log/acme/api/main.log | awk 'LINE_NO != 1'`,
		`cat /log/acme/api/main.log | awk 'EVENT_ID == ""'`,
		`cat /log/acme/api/main.log | awk 'EVENT_ID != ""'`,
		`cat /log/acme/api/main.log | awk 'EVENT_ID ~ /^[0-9a-f-]{36}$/'`,
		`cat /log/acme/api/main.log | awk '{ print EVENT_ID, LINE_NO, $0 }'`,
		`cat /log/acme/api/main.log | awk '{ print LINE_NO }' | sort | uniq -c`,
		`cat /log/acme/api/main.log | jq '$line_no'`,
		`cat /log/acme/api/main.log | jq 'select($line_no == 1)'`,
		`cat /log/acme/api/main.log | jq 'select($event_id != "")'`,
		`cat /log/acme/api/main.log | jq '{e: $event_id, n: $line_no}'`,
		`tail -n 4 /log/acme/api/main.log | awk 'LINE_NO > 0'`,
		`head -n 4 /log/acme/api/main.log | awk '{ print LINE_NO }'`,
	} {
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
	// fieldQueries counts the queries shards were asked in their field form.
	fieldQueries atomic.Int32
	// unchecked skips dispatch's pre-run of each request's SQL, which a
	// benchmark must not pay for: it would add a whole query per iteration.
	unchecked bool
}

// countingShard is a current shard that counts the field-form queries it gets.
type countingShard struct {
	*shard.DuckDBShard
	n *atomic.Int32
}

func (c countingShard) QueryFieldsToSink(ctx context.Context, sql, fieldSQL string, args []any, sink shard.RowSink) {
	c.n.Add(1)
	c.DuckDBShard.QueryFieldsToSink(ctx, sql, fieldSQL, args, sink)
}

func newHarness(t testing.TB, rows []*pb.LogRow, nShards int) *harness {
	t.Helper()
	return newHarnessWith(t, rows, nShards, nil)
}

// oldShard is a shard from before field SQL: the shard.Shard methods only, so
// it has no QueryFieldsToSink and compute runs its plain sql.
type oldShard struct{ shard.Shard }

// newHarnessWith is newHarness where the shards for which old reports true
// are oldShards.
func newHarnessWith(t testing.TB, rows []*pb.LogRow, nShards int, old func(i int) bool) *harness {
	t.Helper()
	h := &harness{engine: &compute.Engine{}}
	for i := 0; i < nShards; i++ {
		sh, err := shard.NewDuckDBShard(fmt.Sprintf("shard-%d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sh.Close() })
		var handle shard.Shard = countingShard{sh, &h.fieldQueries}
		if old != nil && old(i) {
			handle = oldShard{sh}
		}
		h.engine.RegisterResolver(computetest.NewInProcessResolver(sh.ID(), handle))
		h.shards = append(h.shards, sh)
	}
	// Round-robin, so every shard holds a slice of each source.
	for i, sh := range h.shards {
		var part []*pb.LogRow
		for j := i; j < len(rows); j += nShards {
			part = append(part, rows[j])
		}
		if err := sh.Ingest(context.Background(), part, nil); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// check runs a pipeline both ways and compares them. It returns the plan
// under test, or nil when the pipeline isn't plannable.
func (h *harness) check(t *testing.T, pipeline string) *planner.QueryPlan {
	plan, err := planner.Plan(pipeline, 0, "", time.Time{}, time.Time{})
	if err != nil {
		t.Logf("not plannable: %v", err)
		return nil
	}
	source, stages := splitSource(t, pipeline)
	got, gotErr := h.runPlan(plan)
	want, wantErr := h.runReference(source, stages, readsMetaVars(stages))
	switch {
	case gotErr != nil && wantErr != nil:
		// Both fail, e.g. a stage warp doesn't support. Same behavior.
		return plan
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
	return plan
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
		Aggregate:         plan.Aggregate,
		Group:             compute.GroupToProto(plan.Group),
		Limit:             int32(plan.Limit),
		KeepLast:          plan.TailMode,
	})
}

// dispatch runs a request over every shard through the compute engine.
func (h *harness) dispatch(req *pb.ExecuteQueryRequest) (string, error) {
	// A shard that fails its SQL just ends its stream, so check the SQL
	// first or an error would look like an empty result.
	for _, sh := range h.shards {
		for _, sql := range []string{req.Sql, req.FieldSql} {
			if sql == "" || h.unchecked {
				continue
			}
			if _, err := sh.Query(context.Background(), sql, toAny(req.Args)...); err != nil {
				return "", fmt.Errorf("shard SQL %s: %w", sql, err)
			}
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
	sql := strings.Replace(src.DirectShardSQL, "SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no FROM logs",
		"SELECT raw, ts, CAST(event_id AS VARCHAR) AS event_id, line_no, "+cols+"CAST(payload AS VARCHAR) AS "+planner.PayloadColumn+" FROM logs", 1)
	sql = strings.Replace(sql, "SELECT raw, ts, event_id, line_no FROM (",
		"SELECT raw, ts, event_id, line_no, "+cols+planner.PayloadColumn+" FROM (", 1)
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
			if strings.Contains(st, "$"+name) {
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
	for _, p := range docsCorpus(t) {
		add(p)
	}
	for _, p := range generatedCorpus(300) {
		add(p)
	}
	sort.Strings(out)
	return out
}

// docsCorpus is every one-line pipeline in the shell help and the
// manual, its source pointed at a generated source so the stages see
// rows.
func docsCorpus(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{"../../docs/shell", "../../docs/manual"} {
		fs, err := filepath.Glob(dir + "/*.md")
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, fs...)
	}
	if len(files) == 0 {
		t.Fatal("no docs found")
	}
	line := regexp.MustCompile("(?m)(?:^|`|\\$ )((?:cat|head|tail) [^`\n]*\\|[^`\n]*)")
	var out []string
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range line.FindAllStringSubmatch(string(src), -1) {
			p := strings.TrimSpace(m[1])
			if strings.HasSuffix(p, "\\") {
				continue // continues on the next line
			}
			if p = pointAtGeneratedSource(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// pointAtGeneratedSource replaces a pipeline's source paths with one the
// generated rows fill, or returns "" when its source names none.
func pointAtGeneratedSource(pipeline string) string {
	script, err := bash.Parse(pipeline)
	if err != nil || len(script.Commands) != 1 {
		return ""
	}
	cmds := script.Commands[0].First.Commands
	sc, ok := cmds[0].(*bash.SimpleCommand)
	if !ok {
		return ""
	}
	replaced := false
	for i := 1; i < len(sc.Words); i++ {
		w := sc.Words[i].Literal()
		if strings.Contains(w, "$") {
			return ""
		}
		if strings.HasSuffix(w, ".log") || strings.HasPrefix(w, "/log/") {
			sc.Words[i] = bash.Word{Parts: []bash.WordPart{&bash.LiteralPart{Value: "/log/acme/api/main.log"}}}
			replaced = true
		}
	}
	if !replaced {
		return ""
	}
	return bash.Unparse(script)
}

// generatedCorpus composes pipelines of one to three stages from the
// constructs the frontends lower, and from ones they don't, so every
// rule meets its neighbors. Seeded, so the corpus is stable.
func generatedCorpus(n int) []string {
	sources := []string{
		"cat /log/acme/api/main.log", "cat vimgym.log", "cat warp.log",
		"head -n 50 /log/acme/web/main.log", "tail -n 60 callbox.log",
	}
	stages := []string{
		// filters
		`grep timeout`, `grep -v login`, `grep -i error`, `grep -E 'it.s|café'`,
		`sed -n '/ok/p'`, `awk 'LEVEL == "ERROR"'`, `awk '/timeout/'`,
		`awk 'LEVEL != "INFO" && $0 ~ /a/'`, `awk 'LEVEL == "WARN" { print "W" }'`,
		`awk '$2 == "ERROR"'`, `awk 'HOST == "i-001" || LEVEL == ""'`,
		`awk '$5 > 100'`, `awk '$1 ~ /^[0-9]/'`,
		`jq 'select(.status == 500)'`, `jq 'select(.status >= 200)'`,
		`jq 'select(.action == "it'\''s")'`, `jq 'select(.status == 500 and .action == "login")'`,
		`jq 'select($level == "ERROR")'`,
		// projections and transforms
		`awk '{ print LEVEL }'`, `awk '{ print NAMESPACE, $0 }'`, `awk '{ print $2 }'`,
		`awk '{ print $3, $1 }'`,
		`jq '.action'`, `jq -r '.action'`, `jq '{a: .action, s: .status}'`, `jq -c '.foo'`,
		`jq -r '$level'`, `jq 'select(.status == 500) | .action'`, `cut -c 1-12`,
		`jq -r '.status'`, `jq -r '.foo'`, `cut -d ' ' -f 2`, `cut -d, -f1,3-`, `cut -d'"' -f4,2`,
		`sed 's/o/0/g'`, `tr a-z A-Z`,
		// line counts
		`head -n 3`, `tail -n 4`, `head -2`, `awk 'NR >= 2 && NR <= 6'`, `awk 'NR > 1'`,
		// order and aggregates
		`sort`, `sort -r`, `uniq -c`, `wc -l`, `sort -rn`, `uniq`, `sort -u`,
	}
	r := rand.New(rand.NewSource(2))
	var out []string
	for len(out) < n {
		p := sources[r.Intn(len(sources))]
		for k := 1 + r.Intn(3); k > 0; k-- {
			p += " | " + stages[r.Intn(len(stages))]
		}
		out = append(out, p)
	}
	return out
}

var extraCorpus = []string{
	`cat /log/acme/api/main.log | jq 'select(.status == 500)'`,
	`cat /log/acme/api/main.log | jq 'select(.status == 500) | .action'`,
	`cat /log/acme/api/main.log | jq -r 'select(.status == 500) | .action'`,
	`cat /log/acme/api/main.log | jq '{a: .action, s: .status}'`,
	`cat /log/acme/api/main.log | jq '[.status, .action]'`,
	`cat /log/acme/api/main.log | jq '.foo.bar'`,
	`cat /log/acme/api/main.log | jq '$level'`,
	`cat /log/acme/api/main.log | jq 'select($level == "ERROR") | {l: $level, h: $host}'`,
	`tail -n 30 /log/acme/api/main.log | jq 'select(.status == 500)' | wc -l`,
	`head -n 30 /log/acme/api/main.log | jq 'select(.status == 500)'`,
	`cat /log/acme/api/main.log | jq 'select(.status == 500)' | grep login`,
	`cat /log/acme/api/main.log | jq 'select(has("items")) | .items'`,
	// Nested paths: a step through the wrong type errors in jq, which
	// drops the row (.status.x on a number or string, .items.x on an
	// array, .foo[0] on an object); null and missing pass as null.
	`cat /log/acme/api/main.log | jq 'select(.foo.bar == 1)'`,
	`cat /log/acme/api/main.log | jq 'select(.foo.bar < 2)'`,
	`cat /log/acme/api/main.log | jq 'select(.foo.bar != 0)'`,
	`cat /log/acme/api/main.log | jq 'select(.foo.bar.baz == null)'`,
	`cat /log/acme/api/main.log | jq 'select(.status.x == null)'`,
	`cat /log/acme/api/main.log | jq 'select(.status.x != 1)'`,
	`cat /log/acme/api/main.log | jq 'select(.items.x == null)'`,
	`cat /log/acme/api/main.log | jq 'select(.foo[0] == null)'`,
	`cat /log/acme/api/main.log | jq 'select(.items[0] == 1)'`,
	`cat /log/acme/api/main.log | jq 'select(.items[1] == "two")'`,
	`cat /log/acme/api/main.log | jq 'select(.items[5] == null)'`,
	`cat /log/acme/api/main.log | jq 'select(.items[-1] == 3.5)'`,
	`cat /log/acme/api/main.log | jq 'select(.foo.bar >= 1) | select(.status == 500)'`,
	// Selects after path steps test the value the program holds; the
	// stage then emits it.
	// Steps through the wrong type error in jq, which skips the row.
	`cat /log/acme/api/main.log | jq '.status.x'`,
	`cat /log/acme/api/main.log | jq '.items.x'`,
	`cat /log/acme/api/main.log | jq '.foo[0]'`,
	`cat /log/acme/api/main.log | jq '.foo.bar.baz'`,
	`cat /log/acme/api/main.log | jq '.status | select(.x == null)'`,
	`cat /log/acme/api/main.log | jq '.items | select(.x == null)'`,
	`cat /log/acme/api/main.log | jq '.foo | select(.bar == 1)'`,
	`cat /log/acme/api/main.log | jq '.foo | select(.bar < 2) | .bar'`,
	`cat /log/acme/api/main.log | jq '.foo | select(.bar != 0) | .bar'`,
	`cat /log/acme/api/main.log | jq -r '.foo | select(.bar == 1) | .bar'`,
	`cat /log/acme/api/main.log | jq '.status | select(. == 500)'`,
	`cat /log/acme/api/main.log | jq '.status | select(. > 100)'`,
	`cat /log/acme/api/main.log | jq '.nothing | select(.x == null)'`,
	`cat /log/acme/api/main.log | jq '.items | select(.[0] == 1)'`,
	`cat /log/acme/api/main.log | jq '.items | select(.[1] == "two") | .[2]'`,
	`cat /log/acme/api/main.log | jq '.foo | select(.bar == 1) | {x: .bar}'`,
	`cat /log/acme/api/main.log | jq 'select(.status == 500) | .foo | select(.bar == 1)'`,
	`cat /log/acme/api/main.log | jq '.items[-1]'`,
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
	// awk compares numerically when both sides look numeric, a string
	// constant included: "500.0" == "500".
	`cat /log/acme/api/main.log | awk 'LEVEL == "500"'`,
	// $N is fields[N], split at ingest the way awk splits.
	`cat /log/acme/api/main.log | awk '$2 == "ERROR"'`,
	`cat /log/acme/api/main.log | awk '$4 == "warmed"' | wc -l`,
	`cat /log/acme/api/main.log | awk '$5 > 1000'`,
	`cat /log/acme/api/main.log | awk '$5 >= "2500"'`,
	`cat /log/acme/api/main.log | awk '$7 < 3'`,
	`cat /log/acme/api/main.log | awk '$9 == ""'`,
	`cat /log/acme/api/main.log | awk '$1 ~ /^\[/ && $3 !~ /^\[i-00[12]/'`,
	`cat /log/acme/api/main.log | awk '{ print $2, $1 }'`,
	`cat /log/acme/api/main.log | awk '{ print $3 "|" HOST }' | sort | uniq -c`,
	// Aggregates the shard computes: compute sums the shards' counts.
	`cat /log/acme/api/main.log | wc -l`,
	`cat /log/acme/api/main.log | grep no-such-line | wc -l`,
	`cat /log/acme/api/main.log | grep no-such-line | sort | uniq -c`,
	`cat /log/acme/api/main.log | awk '{ print $5 }' | wc -l`,
	`cat vimgym.log | awk '{ print $2 }' | sort | uniq -c`,
	`cat /log/acme/api/main.log | jq -r '.action' | sort | uniq -c | sort -rn`,
	// Top-N: compute orders the groups by count and keeps N.
	`cat /log/acme/api/main.log | jq -r '.action' | sort | uniq -c | sort -rn | head -n 3`,
	`cat /log/acme/api/main.log | jq -r '.action' | sort | uniq -c | sort -rn | head -1`,
	`cat /log/acme/api/main.log | jq -r '.action' | sort | uniq -c | sort -rn | head -n 500`,
	`cat /log/acme/api/main.log | awk '{ print $3 }' | sort | uniq -c | sort -rn | head -n 5`,
	`cat /log/acme/api/main.log | awk '{ print $2 }' | sort -r | uniq -c | sort -nr | head -n 4`,
	`cat vimgym.log | awk '{ print $2 }' | sort | uniq -c | sort -rn | head -n 2 | awk '{ print $2 }'`,
	// wc -c: the bytes of the lines, a newline after each. Multi-line rows,
	// multibyte text and an empty result are the cases a character count or a
	// missing newline would get wrong.
	`cat /log/acme/api/main.log | wc -c`,
	`cat /log/acme/api/main.log | grep no-such-line | wc -c`,
	`cat /log/acme/api/main.log | grep 'café' | wc -c`,
	`cat /log/acme/api/main.log | jq -r '.action' | wc -c`,
	`cat /log/acme/api/main.log | awk '{ print $3 }' | wc -c`,
	`cat /log/acme/api/main.log | wc -c | awk '{ print $1 * 2 }'`,
	`tail -n 60 /log/acme/api/main.log | wc -c`,
	// sort -n's end: the greatest or least number, ties by bytes.
	`cat /log/acme/api/main.log | jq -r '.duration_ms' | sort -n | tail -n 1`,
	`cat /log/acme/api/main.log | jq -r '.duration_ms' | sort -rn | head -n 1`,
	`cat /log/acme/api/main.log | jq -r '.duration_ms' | sort -n | head -n 1`,
	`cat /log/acme/api/main.log | jq -r '.status' | sort -rn | tail -n 1`,
	`cat /log/acme/api/main.log | awk '{ print $5 }' | sort -n | tail -n 1`,
	`cat /log/acme/api/main.log | grep no-such-line | sort -n | tail -n 1`,
	`cat /log/acme/api/main.log | sort -n | tail -n 1`,
	`cat /log/acme/api/main.log | sort -nr | head -1 | awk '{ print $1 }'`,
	// Distinct count: the number of groups.
	`cat /log/acme/api/main.log | sort -u | wc -l`,
	`cat /log/acme/api/main.log | sort | uniq | wc -l`,
	`cat /log/acme/api/main.log | sort -r | uniq | wc -l`,
	`cat /log/acme/api/main.log | jq -r '.action' | sort -u | wc -l`,
	`cat /log/acme/api/main.log | awk '{ print $3 }' | sort | uniq | wc -l`,
	`cat /log/acme/api/main.log | grep no-such-line | sort -u | wc -l`,
	`cat /log/acme/api/main.log | jq -r '.action' | sort -u | wc -l | awk '{ print $1 * 2 }'`,
	`tail -n 60 /log/acme/api/main.log | sort -u | wc -l`,
	// awk's count per key: the key is projected, the shard groups it.
	`cat /log/acme/api/main.log | awk '{ c[$3]++ } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | awk '{ ++n[$2] } END { for (x in n) print x, n[x] }'`,
	`cat /log/acme/api/main.log | awk '{ c[$99]++ } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | awk '{ c[$0]++ } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | awk '{ c[$2 "|" $3]++ } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | awk '{ c[HOST]++ } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | grep -v warmed | awk '{ c[$3]++ } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | grep no-such-line | awk '{ c[$3]++ } END { for (k in c) print k, c[k] }'`,
	`cat vimgym.log | awk '{ c[$2]++ } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | awk '{ c[$3]++ } END { for (k in c) print k, c[k] }' | sort -rn | head -n 3`,
	`cat /log/acme/api/main.log | awk '{ c[$3]++ } END { for (k in c) print k, c[k] }' | awk '{ print $2 }'`,
	// Not a tally: a pattern, a sum, the count first, a computed key, a
	// projection ahead, a pushed line count.
	`cat /log/acme/api/main.log | awk '/warmed/ { c[$3]++ } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | awk '{ c[$3] += $5 } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | awk '{ c[$3]++ } END { for (k in c) print c[k], k }'`,
	`cat /log/acme/api/main.log | awk '{ c[toupper($3)]++ } END { for (k in c) print k, c[k] }'`,
	`cat /log/acme/api/main.log | cut -d ' ' -f 2- | awk '{ c[$1]++ } END { for (k in c) print k, c[k] }'`,
	`tail -n 60 /log/acme/api/main.log | awk '{ c[$3]++ } END { for (k in c) print k, c[k] }'`,
	// $NF, length and grep -E lower: see also TestDifferentialLineShapes.
	`cat /log/acme/api/main.log | awk '{ print $NF }' | sort | uniq -c | sort -rn | head -n 3`,
	`cat /log/acme/api/main.log | awk '$NF == "ingest-stream:"'`,
	`cat /log/acme/api/main.log | awk 'length > 80 { print $3 }'`,
	`cat /log/acme/api/main.log | grep -E 'warm|timeout' | wc -l`,
	`cat /log/acme/api/main.log | grep -Ei 'IT.S|NA[IÏ]VE'`,
	// Stages that stay in compute but read only some fields: the shard sends a
	// record of just those, and the stage must see the same $N, $NF and NF.
	`cat /log/acme/api/main.log | awk '{ s[$3] += $5 } END { for (k in s) print k, s[k] }'`,
	`cat /log/acme/api/main.log | awk '{ if ($NF > m) m = $NF } END { print m }'`,
	`cat /log/acme/api/main.log | awk '{ printf "%s|%d|%s\n", $2, NF, $NF }'`,
	`cat /log/acme/api/main.log | awk '{ n++ } END { print n }'`,
	`cat /log/acme/api/main.log | awk 'BEGIN { print "head" } { n += length($3) } END { print n }'`,
	`cat /log/acme/api/main.log | awk '{ print toupper($2), substr($3, 2) }'`,
	`cat /log/acme/api/main.log | grep -v warmed | awk '{ s += $5 } END { print s }'`,
	`cat /log/acme/api/main.log | awk '$5 > 100 { c[$3]++; t += $5 } END { for (k in c) print k, c[k]; print t }'`,
	`cat /log/acme/api/main.log | awk '{ print HOST, $3 ":" NR }'`,
	`cat /log/acme/api/main.log | awk '{ x = $2; gsub(/a/, "A", x); print x }' | sort | uniq -c`,
	// Not projected: they read the line.
	`cat /log/acme/api/main.log | awk '{ print $0, NF }'`,
	`cat /log/acme/api/main.log | awk '/warmed/ { n++ } END { print n }'`,
	`cat /log/acme/api/main.log | awk '{ gsub(/a/, "A"); print $3 }'`,
	// Not top-N: head -n 0, another order, tail, and a keyed sort.
	`cat /log/acme/api/main.log | jq -r '.action' | sort | uniq -c | sort -rn | head -n 0`,
	`cat /log/acme/api/main.log | jq -r '.action' | sort | uniq -c | sort -n | head -n 3`,
	`cat /log/acme/api/main.log | jq -r '.action' | sort | uniq -c | sort -rn | tail -n 3`,
	`cat /log/acme/api/main.log | jq -r '.action' | sort | uniq -c | sort -rnk2 | head -n 3`,
	`cat /log/acme/api/main.log | jq '.status' | sort -u`,
	`cat /log/acme/api/main.log | jq -r '.status' | sort -r | uniq`,
	`cat /log/acme/api/main.log | cut -d ' ' -f 2 | sort | uniq -c`,
	`cat /log/acme/api/main.log | jq -c '{a: .action}' | sort | uniq -c | head -3`,
	`cat /log/acme/api/main.log | sort -u | uniq -c`,
	`cat /log/acme/api/main.log | awk '{ print LEVEL }' | sort | uniq -c | awk '{ print $2 }'`,
	// Declined: the stage after names a meta-variable; a pushed line
	// count; row stages first.
	`cat /log/acme/api/main.log | sort | uniq -c | awk '{ print LEVEL, $0 }'`,
	`tail -n 60 /log/acme/api/main.log | sort | uniq -c`,
	`cat /log/acme/api/main.log | grep -E 'it.s' | jq 'select($level == "ERROR")' | wc -l`,
	// Projections: cut and jq -r.
	`cat /log/acme/api/main.log | cut -d ' ' -f 3,1`,
	`cat /log/acme/api/main.log | cut -d ' ' -f 2-4`,
	`cat /log/acme/api/main.log | cut -d: -f2-`,
	`cat /log/acme/api/main.log | cut -d'"' -f4,2,9-`,
	`cat /log/acme/api/main.log | cut -f1`,
	`cat /log/acme/api/main.log | cut -d ' ' -f 3-2`,
	`cat /log/acme/api/main.log | jq -r '{a: .action}'`,
	`cat /log/acme/api/main.log | jq -r '.items'`,
	`cat /log/acme/api/main.log | jq -r '.nothing'`,
	`cat /log/acme/api/main.log | jq -r '.success'`,
	`cat /log/acme/api/main.log | jq -r '.duration_ms'`,
	`cat /log/acme/api/main.log | jq -r '[.action, .status]'`,
	`tail -n 40 /log/acme/api/main.log | awk '$2 == "INFO" { print $NF }'`,
	// Filter hoisting: grep and sed below a sort, on each line.
	`cat /log/acme/api/main.log | sort | grep login`,
	`cat /log/acme/api/main.log | sort -r | grep -v INFO | head -5`,
	`cat /log/acme/api/main.log | sort -u | grep -i error | sort -r | grep -v warmed`,
	`cat /log/acme/api/main.log | grep a | sort | sed -n '/login/p' | uniq -c`,
	`cat /log/acme/api/main.log | sort | grep ERROR | wc -l`,
	`cat /log/acme/api/main.log | jq -r .action | sort | grep -v login`,
	// ... and below the SELECT's projection.
	`cat /log/acme/api/main.log | jq -r .action | grep login`,
	`cat /log/acme/api/main.log | jq -r '.action' | grep -v login | sort | uniq -c`,
	`cat /log/acme/api/main.log | jq '.status' | grep 500 | wc -l`,
	`cat /log/acme/api/main.log | cut -d ' ' -f 2 | grep -i info`,
	`cat /log/acme/api/main.log | awk '{ print $2, $3 }' | sed -n '/ERROR/p' | head -4`,
	`cat /log/acme/api/main.log | jq -r .action | grep o | sort -u | grep -v login`,
	`cat /log/acme/api/main.log | grep a | awk '{ print LEVEL }' | grep -v INFO`,
	`cat /log/acme/api/main.log | jq -r '.action' | grep login | awk '{ print LEVEL, $0 }'`,
	// Declined: a keyed sort, a pushed line count, a filter after uniq.
	`cat /log/acme/api/main.log | sort -n | grep login`,
	`tail -n 50 /log/acme/api/main.log | sort | grep login`,
	`cat /log/acme/api/main.log | sort | uniq | grep login`,
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
		level := pick("ERROR", "WARN", "INFO", "DEBUG", "", "500.0")
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

// TestDifferentialNumericExtreme feeds sort -n's end lines built to break a
// number parse that isn't warp's: signs, a bare dot, a second dot, exponents
// and hex (which sort -n ignores), spaces and tabs in front, digits that
// aren't ASCII, a number too big for a float, and numbers that tie so the
// line's bytes decide.
func TestDifferentialNumericExtreme(t *testing.T) {
	huge := strings.Repeat("9", 400)
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for _, set := range [][]string{
		{"5", "5.0", "05", "5abc", "5 ", " 5", "+5", "5."},
		{"-5", "-5.5", "-0", "0", "-0.0", "0.0", "00", ""},
		{".5", "-.5", "+.5", ".", "-.", "+", "-", "1.2.3", "1..2"},
		{"1e5", "1e+3", "0x10", "0x", "9e", "1E3", "1_000", "1,000"},
		{"\t9", "  \t 8", "\n", "a1", "1a", "abc", "ABC", "naïve", "٣", "日本"},
		{huge, "-" + huge, "1" + strings.Repeat("0", 308), "1" + strings.Repeat("0", 309)},
		{"7", "7 b", "7 a", "7\tz", "007", "7.00"},
	} {
		var rows []*pb.LogRow
		for i, raw := range set {
			rows = append(rows, &pb.LogRow{
				Raw: raw, TsUnixNs: base.Add(time.Duration(i) * time.Second).UnixNano(),
				Namespace: "acme", Service: "api", Kind: "main", CustomerId: 1, PayloadJson: []byte("{}"),
			})
		}
		for _, shards := range []int{1, 2, 3} {
			h := newHarness(t, rows, shards)
			for _, p := range []string{
				`cat /log/acme/api/main.log | sort -n | tail -n 1`,
				`cat /log/acme/api/main.log | sort -rn | head -n 1`,
				`cat /log/acme/api/main.log | sort -n | head -n 1`,
				`cat /log/acme/api/main.log | sort -rn | tail -n 1`,
			} {
				t.Run(fmt.Sprintf("%d shards/%q/%s", shards, set[0], p), func(t *testing.T) {
					plan := h.check(t, p)
					if plan == nil || plan.Group == nil {
						t.Fatalf("not grouped in the shard: %+v", plan)
					}
				})
			}
		}
	}
}
