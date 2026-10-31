package compute

import (
	"fmt"
	"strings"
	"testing"
	"time"

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
