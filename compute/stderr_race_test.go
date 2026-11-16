package compute

import (
	"fmt"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// Stages of one pipeline run concurrently, and each writes its errors to the
// same stderr. A stage's write must not race another's (run under -race).
func TestJob_ConcurrentStageErrorsDoNotRaceOnStderr(t *testing.T) {
	for i := 0; i < 20; i++ {
		job := (&Engine{}).NewJob([]string{`wc -l`, `jq 'select(.status == 500)'`, `cut -c 1-12`}, 1, "ts", 0, false, false)
		go func() {
			for n := 0; n < 50; n++ {
				job.SendRow(&pb.ResultRow{TsUnixNs: int64(n), Raw: fmt.Sprintf("line %d", n)}, "a")
			}
			job.SendSentinel("a")
		}()
		// The outcome (an error for the jq stage's input, or none) isn't what
		// this checks; the race detector is.
		_, _ = job.Result()
	}
}
