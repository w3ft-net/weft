package compute

import (
	"strings"
	"sync"
	"testing"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// rowFilterRows are text lines whose JSON lives only in the payload
// column, the shape of an envelope'd or prefixed source.
func rowFilterRows() []*pb.ResultRow {
	row := func(ts int64, raw, payload, level string) *pb.ResultRow {
		return &pb.ResultRow{TsUnixNs: ts, Raw: raw, MetaVars: map[string]string{planner.PayloadColumn: payload, "level": level}}
	}
	return []*pb.ResultRow{
		row(1, "10:00 api login failed", `{"status":500}`, "ERROR"),
		row(2, "10:01 api login ok", `{"status":200}`, "INFO"),
		row(3, "10:02 api search failed", `{"status":500}`, "ERROR"),
		row(4, "10:03 api plain text", `{}`, "INFO"),
		row(5, "10:04 api update failed", `{"status":500}`, "WARN"),
	}
}

func runRowStagesJob(t *testing.T, rowStages, stages []string, limit int, keepLast bool) string {
	t.Helper()
	job := (&Engine{}).NewJob(stages, 1, "ts", limit, keepLast, false)
	rs, err := NewRowStages(rowStages)
	if err != nil {
		t.Fatal(err)
	}
	job.rowStages = rs
	for _, r := range rowFilterRows() {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")
	out, err := job.Result()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func runRowFilterJob(t *testing.T, stages []string, limit int, keepLast bool, program string) string {
	t.Helper()
	return runRowStagesJob(t, []string{"jq '" + program + "'"}, stages, limit, keepLast)
}

func TestRowStages_JqKeepsRawLinesWhosePayloadMatches(t *testing.T) {
	got := runRowFilterJob(t, nil, 0, false, `select(.status == 500)`)
	want := "10:00 api login failed\n10:02 api search failed\n10:04 api update failed"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestRowStages_JqReadsMetaVars(t *testing.T) {
	got := runRowFilterJob(t, nil, 0, false, `select(._level == "ERROR")`)
	want := "10:00 api login failed\n10:02 api search failed"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// A select that names no meta-variable reads the payload as it is, so
// `keys` sees only the payload's own.
func TestRowStages_JqWithoutMetaVarSeesNoSplicedKeys(t *testing.T) {
	got := runRowFilterJob(t, nil, 0, false, `select(keys == ["status"])`)
	want := "10:00 api login failed\n10:01 api login ok\n10:02 api search failed\n10:04 api update failed"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

// `head -n 2 | jq 'select(...)'` filters the first two rows; the limit
// counts rows before the filter, not survivors.
func TestRowStages_JqRunsAfterTheHeadLimit(t *testing.T) {
	got := runRowFilterJob(t, nil, 2, false, `select(.status == 500)`)
	if got != "10:00 api login failed" {
		t.Errorf("got %q", got)
	}
}

// `tail -n 2 | jq 'select(...)'` filters the last two rows.
func TestRowStages_JqRunsAfterTheTailLimit(t *testing.T) {
	got := runRowFilterJob(t, nil, 2, true, `select(.status == 500)`)
	if got != "10:04 api update failed" {
		t.Errorf("got %q", got)
	}
}

func TestRowStages_JqFeedsLaterStages(t *testing.T) {
	if got := runRowFilterJob(t, []string{"wc -l"}, 0, false, `select(.status == 500)`); got != "3" {
		t.Errorf("wc -l = %q, want 3", got)
	}
	if got := runRowFilterJob(t, []string{"wc -l"}, 2, true, `select(.status == 500)`); got != "1" {
		t.Errorf("tail-mode wc -l = %q, want 1", got)
	}
}

// jq stages print compact JSON, one value per line.
func TestJob_JqIsCompactByDefault(t *testing.T) {
	job := (&Engine{}).NewJob([]string{"jq '{s: .status}'"}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1, Raw: `{"status":500}`}, "a")
	job.SendSentinel("a")
	out, err := job.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != `{"s":500}` {
		t.Errorf("got %q", got)
	}
}

func TestRowStages_FiltersReadTheRawLineAndMetaVars(t *testing.T) {
	if got := runRowStagesJob(t, []string{"grep failed", `awk 'LEVEL == "ERROR"'`}, nil, 0, false); got != "10:00 api login failed\n10:02 api search failed" {
		t.Errorf("grep | awk LEVEL: got %q", got)
	}
	if got := runRowStagesJob(t, []string{"sed -n '/login/p'"}, nil, 0, false); got != "10:00 api login failed\n10:01 api login ok" {
		t.Errorf("sed -n: got %q", got)
	}
}

// Rows survive the filters whole, so the jq after them reads the payload.
func TestRowStages_ThenAStageReadingThePayload(t *testing.T) {
	job := (&Engine{}).NewJob([]string{"jq -c '.status'"}, 1, "ts", 0, false, false)
	rs, err := NewRowStages([]string{"grep failed"})
	if err != nil {
		t.Fatal(err)
	}
	job.rowStages, job.stagesReadPayload = rs, true
	for _, r := range rowFilterRows() {
		job.SendRow(r, "a")
	}
	job.SendSentinel("a")
	out, err := job.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != "500\n500\n500" {
		t.Errorf("got %q", got)
	}
}

func TestRowStages_HeadAndTailCountSurvivors(t *testing.T) {
	if got := runRowStagesJob(t, []string{"grep api", "tail -n 2"}, nil, 0, false); got != "10:03 api plain text\n10:04 api update failed" {
		t.Errorf("tail: got %q", got)
	}
	if got := runRowStagesJob(t, []string{"head -2", `awk 'LEVEL == "ERROR"'`}, nil, 0, false); got != "10:00 api login failed" {
		t.Errorf("head: got %q", got)
	}
	if got := runRowStagesJob(t, []string{"tail -n 3", "head -n 1"}, []string{"wc -l"}, 0, false); got != "1" {
		t.Errorf("tail | head | wc: got %q", got)
	}
}

// BenchmarkRowStageKeep measures one row through a text row stage, which
// builds an interpreter per row, against a jq select chain, which doesn't.
func BenchmarkRowStageKeep(b *testing.B) {
	raw := `2026-10-14T12:00:00Z ERROR request failed status=500 path=/api/orders/123`
	payload := `{"status":500,"path":"/api/orders/123"}`
	for name, stage := range map[string]string{
		"grep": `grep ERROR`,
		"sed":  `sed -n '/ERROR/p'`,
		"awk":  `awk '$2 == "ERROR"'`,
		"jq":   `jq 'select(.status == 500)'`,
	} {
		rs, err := NewRowStages([]string{stage})
		if err != nil {
			b.Fatal(err)
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if !rs.Keep(raw, payload, nil) {
					b.Fatal("row dropped")
				}
			}
		})
	}
}

// TestRowStages_ReusedInterpreterSeesEachRowsMeta: a stage's interpreter is
// reused across rows, so an awk reading a meta-variable must get the
// current row's value, not the first row's, and a dropped row mustn't
// leak output into the next.
func TestRowStages_ReusedInterpreterSeesEachRowsMeta(t *testing.T) {
	rs, err := NewRowStages([]string{`awk 'HOST == "b"'`})
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		host string
		want bool
	}{{"a", false}, {"b", true}, {"a", false}, {"b", true}, {"b", true}, {"c", false}} {
		if got := rs.Keep("line", "", map[string]string{"instance": tc.host}); got != tc.want {
			t.Fatalf("row %d host %q: Keep = %v, want %v", i, tc.host, got, tc.want)
		}
	}

	g, err := NewRowStages([]string{`grep ERROR`})
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		raw  string
		want bool
	}{{"ERROR x", true}, {"ok", false}, {"ok", false}, {"ERROR y", true}} {
		if got := g.Keep(tc.raw, "", nil); got != tc.want {
			t.Fatalf("row %d %q: Keep = %v, want %v", i, tc.raw, got, tc.want)
		}
	}
}

// TestRowStages_KeepIsConcurrencySafe: the live path calls Keep from many
// subscribers at once; run with -race.
func TestRowStages_KeepIsConcurrencySafe(t *testing.T) {
	rs, err := NewRowStages([]string{`grep ERROR`, `awk 'HOST == "b"'`})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				host, raw := "a", "ok"
				want := false
				if i%2 == 0 {
					host, raw, want = "b", "ERROR x", true
				}
				if got := rs.Keep(raw, "", map[string]string{"instance": host}); got != want {
					t.Errorf("Keep(%q, %q) = %v, want %v", raw, host, got, want)
					return
				}
			}
		}()
	}
	wg.Wait()
}
