package compute

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// The writer batches rows into the stage's stdin, but each row's meta-variable
// values must stay with its own record: across the batches a flush cuts, and
// past the 1024 the per-record queue holds.
func TestJob_AwkMetaVarsStayWithTheirRowAcrossBatches(t *testing.T) {
	const n = 5000
	job := (&Engine{}).NewJob([]string{`awk '{ print LEVEL, $2 }'`}, 1, "ts", 0, false, false)
	go func() {
		for i := 0; i < n; i++ {
			job.SendRow(&pb.ResultRow{
				TsUnixNs: int64(i), Raw: fmt.Sprintf("row %d", i),
				MetaVars: map[string]string{"level": fmt.Sprintf("L%d", i)},
			}, "a")
		}
		job.SendSentinel("a")
	}()
	done := make(chan string, 1)
	go func() { out, _ := job.Result(); done <- out }()
	select {
	case out := <-done:
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		if len(lines) != n {
			t.Fatalf("%d lines, want %d", len(lines), n)
		}
		for i, l := range lines {
			if want := fmt.Sprintf("L%d %d", i, i); l != want {
				t.Fatalf("line %d = %q, want %q", i, l, want)
			}
		}
	case <-time.After(20 * time.Second):
		t.Fatal("job deadlocked")
	}
}

// A slow producer's rows reach the stage as they come; the writer does not sit
// on them until a buffer fills.
func TestJob_StagedRowsAreNotHeldBackByBatching(t *testing.T) {
	job := (&Engine{}).NewJob([]string{`awk '{ print $1 }'`}, 1, "", 0, false, false)
	stream := job.Stream()
	job.SendRow(&pb.ResultRow{TsUnixNs: 1, Raw: "first row"}, "a")

	got := make(chan string, 1)
	go func() {
		line, _, _ := stream.Next(t.Context())
		got <- line.Raw
	}()
	select {
	case line := <-got:
		if line != "first" {
			t.Errorf("first line = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first row's output waited for more rows")
	}
	job.SendSentinel("a")
}

// A stage that stops reading early (head) doesn't strand the writer, which
// drains the rest of the rows without emitting.
func TestJob_EarlyExitStageDoesNotStrandTheWriter(t *testing.T) {
	job := (&Engine{}).NewJob([]string{`head -n 2`}, 1, "ts", 0, false, false)
	go func() {
		for i := 0; i < 200000; i++ {
			job.SendRow(&pb.ResultRow{TsUnixNs: int64(i), Raw: fmt.Sprintf("row %d", i)}, "a")
		}
		job.SendSentinel("a")
	}()
	done := make(chan string, 1)
	go func() { out, _ := job.Result(); done <- out }()
	select {
	case out := <-done:
		if out != "row 0\nrow 1\n" {
			t.Errorf("head -n 2 = %q", out)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("job stranded after head stopped reading")
	}
}

// A jq stage that names no meta-variable reads its input as it is. A payload
// that merely looks like the envelope WrapJqInput makes is the user's data, not
// ours to unwrap.
func TestJob_JqWithoutMetaVarsLeavesAnEnvelopeShapedPayloadAlone(t *testing.T) {
	const payload = `{"__weft_vars__":{"level":"x"},"__weft_input__":{"b":2}}`
	job := (&Engine{}).NewJob([]string{`jq -c .`}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1, Raw: payload}, "a")
	job.SendSentinel("a")
	out, err := job.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out); got != payload {
		t.Errorf("jq -c . = %q, want the payload unchanged %q", got, payload)
	}
}

// A row stage builds a row's meta-variables only if it reads them.
func TestRowRunner_BuildsMetaVarsOnlyForAStageThatReadsThem(t *testing.T) {
	allocs := func(stage string) float64 {
		rs, err := NewRowStages([]string{stage})
		if err != nil {
			t.Fatal(err)
		}
		r := rs.runner(func(*pb.ResultRow) bool { return true })
		row := &pb.ResultRow{TsUnixNs: 1, Raw: "x y", MetaVars: map[string]string{"level": "ERROR", "instance": "i-1"}}
		return testing.AllocsPerRun(200, func() { r.push(0, row) })
	}
	plain, reads := allocs(`awk '$1 == "x"'`), allocs(`awk 'LEVEL == "ERROR"'`)
	if reads-plain < 5 {
		t.Errorf("%.0f allocations a row for a stage that reads no meta-variable, %.0f for one that does: the plain one still builds them", plain, reads)
	}
}

func TestRowStages_ReadsMeta(t *testing.T) {
	for stages, want := range map[string]bool{
		`grep x`:                         false,
		`head -n 2`:                      false,
		`awk '$1 == "x"'`:                false,
		`jq 'select(.a == 1)'`:           false,
		`awk 'LEVEL == "ERROR"'`:         true,
		`jq 'select($level == "ERROR")'`: true,
	} {
		rs, err := NewRowStages([]string{stages})
		if err != nil {
			t.Fatalf("%s: %v", stages, err)
		}
		if got := rs.ReadsMeta(); got != want {
			t.Errorf("%s: ReadsMeta = %v, want %v", stages, got, want)
		}
	}
}

// A stage's meta-variables are built from the columns it reads and no others;
// a row carries about twenty.
func TestMetaVars_OnlyWhatTheStageReads(t *testing.T) {
	row := &pb.ResultRow{
		TsUnixNs: 1000, EventId: "ev", LineNo: 4, Raw: "x",
		MetaVars: map[string]string{"level": "ERROR", "instance": "web-1", "pod": "p-1", "deployment": "d-1", "duration_ns": "1500"},
	}
	for _, c := range []struct {
		stage string
		keys  []string
	}{
		{`awk 'LEVEL == "ERROR"'`, []string{"level"}},
		{`awk '{ print HOST, POD }'`, []string{"instance", "pod"}},
		{`awk '{ print TS }'`, []string{"ts"}},
		{`jq -c '[$level, $line_no]'`, []string{"level", "line_no"}},
		{`awk -f prog.awk`, []string{"level", "instance", "pod", "deployment", "duration_ns", "ts", "event_id", "line_no"}},
	} {
		read := planner.StageMetaRead(c.stage)
		got := rowMetaVars(row, read)
		if len(got) != len(c.keys) {
			t.Errorf("%s: built %v, want just %v", c.stage, got, c.keys)
		}
		for _, k := range c.keys {
			if _, ok := got[k]; !ok {
				t.Errorf("%s: %s missing from %v", c.stage, k, got)
			}
		}
	}

	hostOnly := newAwkBinder(planner.StageMetaRead(`awk '{ print HOST }'`))
	vars := hostOnly.vars(rowMetaVars(row, planner.StageMetaRead(`awk '{ print HOST }'`)))
	if len(vars) != 2 || vars["HOST"] != "web-1" || vars["INSTANCE"] != "web-1" {
		t.Errorf("a stage reading HOST binds %v, want HOST and INSTANCE only", vars)
	}

	level := NewJqBinder(planner.StageMetaRead(`jq -c '$level'`))
	if vars := level.Vars(rowMetaVars(row, planner.StageMetaRead(`jq -c '$level'`))); len(vars) != 1 || vars["level"].Str != "ERROR" {
		t.Errorf("a stage reading $level binds %v, want only level", vars)
	}
}

// The values a stage reads are the row's own, however many columns it skips.
func TestJob_MetaVarsReadFromAWideRow(t *testing.T) {
	for stage, want := range map[string]string{
		`awk '{ print LEVEL, HOST, $2 }'`:       "ERROR web-1 b",
		`jq -r '"\($level) \($host) \(.k)"'`:    "ERROR web-1 v",
		`jq -r '"\($duration_ns) \($line_no)"'`: "1500 4",
	} {
		job := (&Engine{}).NewJob([]string{stage}, 1, "ts", 0, false, false)
		raw := "a b"
		if strings.HasPrefix(stage, "jq") {
			raw = `{"k":"v"}`
		}
		job.SendRow(&pb.ResultRow{
			TsUnixNs: 1000, EventId: "ev", LineNo: 4, Raw: raw,
			MetaVars: map[string]string{"level": "ERROR", "instance": "web-1", "pod": "p", "deployment": "d", "duration_ns": "1500", "region": "r"},
		}, "a")
		job.SendSentinel("a")
		out, err := job.Result()
		if err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("%s = %q, want %q", stage, got, want)
		}
	}
}

// A jq stage's meta-variables stay with their own row, though some rows have no
// payload (nothing for jq to read, so nothing to bind) and the stream is far
// longer than anything buffered.
func TestJob_JqMetaVarsStayWithTheirRow(t *testing.T) {
	const n = 6000
	job := (&Engine{}).NewJob([]string{`jq -r '"\($level) \(.i)"'`}, 1, "ts", 0, false, false)
	go func() {
		for i := 0; i < n; i++ {
			raw := fmt.Sprintf(`{"i":%d}`, i)
			if i%7 == 3 {
				raw = "  " // no payload
			}
			job.SendRow(&pb.ResultRow{TsUnixNs: int64(i), Raw: raw, MetaVars: map[string]string{"level": fmt.Sprintf("L%d", i)}}, "a")
		}
		job.SendSentinel("a")
	}()
	done := make(chan string, 1)
	go func() { out, _ := job.Result(); done <- out }()
	select {
	case out := <-done:
		lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
		k := 0
		for i := 0; i < n; i++ {
			if i%7 == 3 {
				continue
			}
			if want := fmt.Sprintf("L%d %d", i, i); k >= len(lines) || lines[k] != want {
				t.Fatalf("line %d = %q, want %q", k, lines[min(k, len(lines)-1)], want)
			}
			k++
		}
		if k != len(lines) {
			t.Errorf("%d lines, want %d", len(lines), k)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("job deadlocked")
	}
}

// A payload that is not exactly one JSON value fails the query, as it always
// has; it must not be read as a value of its own and shift every later row's
// variables onto the wrong row.
func TestJob_JqPayloadOfTwoValuesFailsAndDoesNotMisbind(t *testing.T) {
	for _, bad := range []string{`{"i":1} {"i":2}`, `{"i":`, `1 2`} {
		job := (&Engine{}).NewJob([]string{`jq -r '"\($level) \(.i)"'`}, 1, "ts", 0, false, false)
		job.SendRow(&pb.ResultRow{TsUnixNs: 1, Raw: `{"i":0}`, MetaVars: map[string]string{"level": "A"}}, "a")
		job.SendRow(&pb.ResultRow{TsUnixNs: 2, Raw: bad, MetaVars: map[string]string{"level": "B"}}, "a")
		job.SendRow(&pb.ResultRow{TsUnixNs: 3, Raw: `{"i":3}`, MetaVars: map[string]string{"level": "C"}}, "a")
		job.SendSentinel("a")
		out, err := job.Result()
		if err == nil {
			t.Errorf("payload %q: no error, output %q", bad, out)
		}
		if strings.Contains(out, "B ") || strings.Contains(out, "C ") {
			t.Errorf("payload %q: output %q has a row after the bad one, bound to variables of its own or another's", bad, out)
		}
	}
}

// A first stage that reads no input, with a later stage keeping the pipeline
// alive, leaves the writer's variables unread. The pipe holds a megabyte, so
// the writer gets ahead and fills the queue; it must not wait on it forever.
func TestJob_StageThatReadsNoInputDoesNotStrandTheWriter(t *testing.T) {
	// The second stage keeps the pipeline running for a while after the first
	// has exited without reading a row.
	const busy = `awk 'BEGIN { for (i = 0; i < 3000000; i++) x += i } { print }'`
	for _, stages := range [][]string{
		{`awk 'BEGIN { print HOST }'`, busy},
		{`jq -n '$level'`, busy},
		{`jq -n '1'`, busy},
	} {
		job := (&Engine{}).NewJob(stages, 1, "ts", 0, false, false)
		go func() {
			for i := 0; i < 5000; i++ {
				job.SendRow(&pb.ResultRow{TsUnixNs: int64(i), Raw: `{"i":1}`, MetaVars: map[string]string{"level": "x", "instance": "h"}}, "a")
			}
			job.SendSentinel("a")
		}()
		done := make(chan struct{})
		go func() { job.Result(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("%v: the writer waited on variables the stage never read", stages)
		}
	}
}
