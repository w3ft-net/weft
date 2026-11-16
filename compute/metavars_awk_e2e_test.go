package compute

import (
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// TestJob_AwkMetaVarOutputRead is the end-to-end regression
// test for awk's half of "reserved meta-variables ... resolve
// incorrectly when read as output":
// runs a real awk stage through warp's bash.RunStreamingWithAwkHook
// (not just awkVarsFromMeta in isolation) and asserts a bare HOST read
// resolves to the row's actual instance column.
func TestJob_AwkMetaVarOutputRead(t *testing.T) {
	pool := &Engine{}
	job := pool.NewJob([]string{`awk '{ print HOST }'`}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{
		TsUnixNs: 1000,
		Raw:      "some log line",
		MetaVars: map[string]string{"instance": "web-1"},
	}, "a")
	job.SendSentinel("a")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := strings.TrimSpace(out); got != "web-1" {
		t.Errorf("awk '{ print HOST }' = %q, want %q", got, "web-1")
	}
}

// A row with no MetaVars (the overwhelming common case — most queries
// have no awk stage reading a meta-variable) must pass through
// unmodified: HOST is genuinely unset, so it reads as awk's ordinary
// uninitialized value (empty string) — unlike the old "$HOST" syntax,
// which evaluated to $0 (the whole record) instead of empty. This pins that the syntax
// migration actually fixed the unset case's failure mode too, not
// just the meta-var-set case.
func TestJob_AwkMetaVarOutputRead_NoMetaVarsIsEmpty(t *testing.T) {
	pool := &Engine{}
	job := pool.NewJob([]string{`awk '{ print HOST }'`}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: "some log line"}, "a")
	job.SendSentinel("a")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := strings.TrimSpace(out); got != "" {
		t.Errorf("awk '{ print HOST }' with no MetaVars = %q, want empty", got)
	}
}

// A plain awk stage that never references a meta-variable must not pay
// for the hook machinery at all — pins that firstStageIsAwk gating
// doesn't change ordinary awk behavior.
func TestJob_AwkOrdinaryStageUnaffected(t *testing.T) {
	pool := &Engine{}
	job := pool.NewJob([]string{`awk '{ print $1 }'`}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1000, Raw: "one two three"}, "a")
	job.SendSentinel("a")

	out, err := job.Result()
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if got := strings.TrimSpace(out); got != "one" {
		t.Errorf("awk '{ print $1 }' = %q, want %q", got, "one")
	}
}

// Meta-variables belong to shard rows, which only the first stage reads.
// Two adjacent awk stages, or an awk after another stage, must run to
// completion; the per-row values feed only the first stage.
func TestJob_AwkMetaVarsOnlyFeedTheFirstStage(t *testing.T) {
	for _, stages := range [][]string{
		{`awk '{ print LEVEL, $0 }'`, `awk '{ print $1 }'`},
		{`grep x`, `awk '{ print LEVEL }'`},
	} {
		job := (&Engine{}).NewJob(stages, 1, "ts", 0, false, false)
		done := make(chan string, 1)
		go func() {
			out, _ := job.Result()
			done <- out
		}()
		go func() {
			for i := 0; i < 3000; i++ {
				job.SendRow(&pb.ResultRow{TsUnixNs: int64(i), Raw: fmt.Sprintf("x row %d", i), MetaVars: map[string]string{"level": "ERROR"}}, "a")
			}
			job.SendSentinel("a")
		}()
		select {
		case out := <-done:
			lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
			if len(lines) != 3000 {
				t.Errorf("%v: %d lines, want 3000", stages, len(lines))
			}
			if stages[0] != `grep x` && lines[0] != "ERROR" {
				t.Errorf("%v: first line %q, want ERROR", stages, lines[0])
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%v: job deadlocked", stages)
		}
	}
}

// Meta-variables reach jq by being spliced into the JSON it reads; no
// other stage may see them in the row's text. An awk reading a JSON
// line's meta-variables prints the line as it was.
func TestJob_MetaVarsSpliceOnlyForAJqFirstStage(t *testing.T) {
	raw := `{"a":1}`
	for stages, want := range map[string]string{
		`awk 'LEVEL == "ERROR"'`: raw,
		`grep a`:                 raw,
		`jq -c '$level'`:         `"ERROR"`,
	} {
		job := (&Engine{}).NewJob([]string{stages}, 1, "ts", 0, false, false)
		job.SendRow(&pb.ResultRow{TsUnixNs: 1, Raw: raw, MetaVars: map[string]string{"level": "ERROR"}}, "a")
		job.SendSentinel("a")
		out, err := job.Result()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out); got != want {
			t.Errorf("%s: got %q, want %q", stages, got, want)
		}
	}
}

// awk reads a record per line, so a row holding several lines feeds it
// several records, each reading its row's meta-variables. One set per
// row would leave the second line's record waiting on the next row's,
// which the writer only sends once awk reads that row: a deadlock.
func TestJob_AwkMetaVarsPerLineOfAMultiLineRow(t *testing.T) {
	job := (&Engine{}).NewJob([]string{`awk '{ print LEVEL, $0 }'`}, 1, "ts", 0, false, false)
	done := make(chan string, 1)
	go func() {
		out, _ := job.Result()
		done <- out
	}()
	for i, r := range []struct{ raw, level string }{
		{"a\nb", "ERROR"}, {"c", "INFO"}, {"d\n", "WARN"}, {"", "DEBUG"},
	} {
		job.SendRow(&pb.ResultRow{TsUnixNs: int64(i), Raw: r.raw, MetaVars: map[string]string{"level": r.level}}, "a")
	}
	job.SendSentinel("a")
	select {
	case out := <-done:
		if want := "ERROR a\nERROR b\nINFO c\nWARN d\nWARN \nDEBUG \n"; out != want {
			t.Errorf("got %q, want %q", out, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("job deadlocked")
	}
}

// A row with more lines than awkVarsCh holds still runs to completion.
func TestJob_AwkMetaVarsRowLongerThanTheQueue(t *testing.T) {
	job := (&Engine{}).NewJob([]string{`awk '{ print LEVEL }'`}, 1, "ts", 0, false, false)
	done := make(chan string, 1)
	go func() {
		out, _ := job.Result()
		done <- out
	}()
	job.SendRow(&pb.ResultRow{TsUnixNs: 1, Raw: strings.Repeat("x\n", 2999) + "x", MetaVars: map[string]string{"level": "ERROR"}}, "a")
	job.SendRow(&pb.ResultRow{TsUnixNs: 2, Raw: "y", MetaVars: map[string]string{"level": "INFO"}}, "a")
	job.SendSentinel("a")
	select {
	case out := <-done:
		if want := strings.Repeat("ERROR\n", 3000) + "INFO\n"; out != want {
			t.Errorf("got %d bytes, want %d", len(out), len(want))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("job deadlocked")
	}
}

// awk keeps a global's last value, so a row whose column is NULL (absent
// from MetaVars) must not read the previous row's value, and a numeric
// one must still compare equal to 0.
func TestJob_AwkMetaVarNullDoesNotKeepThePreviousRowsValue(t *testing.T) {
	job := (&Engine{}).NewJob([]string{`awk '{ print $0 ":" TRACE_ID ":" DURATION_NS ":" (DURATION_NS == 0) }'`}, 1, "ts", 0, false, false)
	job.SendRow(&pb.ResultRow{TsUnixNs: 1, Raw: "a", MetaVars: map[string]string{"trace_id": "t1", "duration_ns": "500"}}, "a")
	job.SendRow(&pb.ResultRow{TsUnixNs: 2, Raw: "b"}, "a")
	job.SendRow(&pb.ResultRow{TsUnixNs: 3, Raw: "c", MetaVars: map[string]string{"trace_id": "t3"}}, "a")
	job.SendSentinel("a")
	out, err := job.Result()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(out), "a:t1:500:0\nb:::1\nc:t3::1"; got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
