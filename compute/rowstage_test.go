package compute

import (
	"strings"
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
