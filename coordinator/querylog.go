package coordinator

import (
	"log"
	"os"
	"path/filepath"
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
var queryLog = log.New(os.Stderr, "", log.LstdFlags)

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
	queryLog.SetOutput(f)
	return nil
}
