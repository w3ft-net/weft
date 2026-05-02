package coordinator

import (
	"context"
	"testing"
)

// drainExec drains an ExecuteResult into the buffered DrainedResult
// shape for tests that assert against Rows/Text/Stderr. Production
// callers (the gRPC server) consume Stream directly and do not call
// this helper.
func drainExec(t *testing.T, res *ExecuteResult) *DrainedResult {
	t.Helper()
	if res == nil {
		return nil
	}
	d, err := res.Drain(context.Background())
	if err != nil {
		t.Fatalf("drain ExecuteResult: %v", err)
	}
	return d
}
