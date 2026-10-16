package coordinator

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/graybeardcorp/weft/internal/weftlog"
)

// queryLog carries the query-audit stream (every "query:" line). It is
// kept off the coordinator's main stderr/journal so substring monitors
// watching weft/coordinator can't match the audit probe that produced
// them — see docs/plans/coordinator-query-audit-split.md. Defaults to
// stderr until SetQueryLog redirects it to a dedicated file.
//
// Flags match the stdlib default logger (date+time) so the audit
// pseudofile's lines keep the `2006/01/02 15:04:05 query: ...` shape
// weft's schema detection already keys timestamps off.
var queryLog = weftlog.NewLogger(queryOut)

// queryOut is where queryLog writes; SetQueryLog swaps it for the audit file.
var queryOut = &switchWriter{w: os.Stderr}

type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Set(w io.Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.w = w
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// SetQueryLog redirects the query-audit stream to path, creating the
// parent directory and opening the file append/create. On any failure
// the logger is left on stderr and the error is returned so the caller
// can warn — audit lines are never lost and the coordinator never
// crashes over log routing.
func SetQueryLog(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	queryOut.Set(f)
	return nil
}

// queryLogCoordinatorID tags every query-audit line via logQuery below.
// Empty (the zero value, e.g. in tests that never call
// SetQueryLogCoordinatorID) omits the field entirely rather than
// printing "coordinator_id=".
var queryLogCoordinatorID string

// SetQueryLogCoordinatorID tags every subsequent query-audit line with
// this coordinator's identity — the same value used for lease-holder
// identity (coordinatorHolder in cmd/coordinator/main.go). Without
// this, a query-routing imbalance across coordinator_desired>1
// instances is invisible from logs alone: heartbeats and runtime-stats
// already carry coordinator_id (HeartbeatWithFields/
// RuntimeStatsWithFields), but queries.log didn't. See
// docs/known-issues.md.
func SetQueryLogCoordinatorID(id string) {
	queryLogCoordinatorID = id
}

// logQuery writes a query-audit line, appending coordinator_id when set
// and, through ctx, the request's trace ID. The sole entry point for
// queryLog so every audit line picks up the tags uniformly — see
// SetQueryLogCoordinatorID.
func logQuery(ctx context.Context, format string, args ...any) {
	l := queryLog
	if queryLogCoordinatorID != "" {
		l = l.With("coordinator_id", queryLogCoordinatorID)
	}
	l.InfoContext(ctx, fmt.Sprintf(format, args...))
}
