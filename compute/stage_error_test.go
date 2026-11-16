package compute

import (
	"context"
	"strings"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// drainJob feeds rows through a one-shard job running stages and returns
// the emitted lines and the stream's terminal error.
func drainJob(t *testing.T, stages []string, rows ...string) ([]string, error) {
	t.Helper()
	pool := &Engine{}
	job := pool.NewJob(stages, 1, "ts", 0, false, false)
	go func() {
		for i, r := range rows {
			job.SendRow(&pb.ResultRow{LineNum: int64(i + 1), Raw: r, TsUnixNs: int64(i + 1)}, "a")
		}
		job.SendSentinel("a")
	}()
	stream := job.Stream()
	defer stream.Close()
	var out []string
	for {
		line, ok, err := stream.Next(context.Background())
		if err != nil {
			return out, err
		}
		if !ok {
			return out, nil
		}
		out = append(out, line.Raw)
	}
}

// A stage that fails must fail the query even when a later stage
// succeeds. Warp reports a pipeline's status as the last stage's, so
// `awk '<bad>' | wc -l` used to exit 0 from wc and the awk error was
// dropped: the user saw "0" instead of the syntax error.
func TestStageError_NotHiddenByLaterStage(t *testing.T) {
	for _, stages := range [][]string{
		{"awk '{ bad syntax'"},
		{"awk '{ bad syntax'", "wc -l"},
		{"awk '{ bad syntax'", "sort", "wc -l"},
	} {
		_, err := drainJob(t, stages, "a", "b")
		if err == nil || !strings.Contains(err.Error(), "syntax error") {
			t.Errorf("stages %v: want the awk syntax error, got %v", stages, err)
		}
	}
}

// Stages that exit non-zero without writing to stderr are not errors:
// `grep` with no match exits 1 and the count after it is a valid 0.
func TestStageError_SilentNonZeroIsNotAnError(t *testing.T) {
	out, err := drainJob(t, []string{"grep nomatch", "wc -l"}, "a", "b")
	if err != nil {
		t.Fatalf("grep with no match then wc: %v", err)
	}
	if len(out) != 1 || strings.TrimSpace(out[0]) != "0" {
		t.Errorf("got %q, want a single 0", out)
	}
}

// An early-exiting consumer closes the pipe under the stages feeding it.
// With pipefail that must not turn into a spurious error: `head -n 1`
// after a stage still writing is a normal query. Kept small on purpose:
// drainJob's producer has no cancellation, so once compute stops reading
// (the pipeline ended early) a producer with more than the job channel's
// buffer left to send blocks forever. That is a test-harness limit, not
// seen live: the coordinator path handled a 50k-row `... | head -n 1`.
func TestStageError_EarlyExitingConsumerIsNotAnError(t *testing.T) {
	rows := make([]string, 200)
	for i := range rows {
		rows[i] = "row " + strings.Repeat("x", 40)
	}
	for _, stages := range [][]string{
		{"awk '{ print }'", "head -n 1"},
		{"sort", "head -n 1"},
		{"grep row", "awk '{ print $1 }'", "head -n 1"},
		{"cat", "head -n 1", "wc -l"},
	} {
		out, err := drainJob(t, stages, rows...)
		if err != nil {
			t.Errorf("stages %v: spurious error %v", stages, err)
		}
		if len(out) != 1 {
			t.Errorf("stages %v: got %d lines, want 1", stages, len(out))
		}
	}
}
