// Package chart implements weft's `chart` builtin and pipe-stage,
// plus the live-redraw View triggered by `chart <watch> --follow`.
//
// It registers into shellcore via Register at construction time:
//
//	cv := chart.NewView(watchResultsProvider)
//	core.RegisterView(cv)
//	core.RegisterBuiltin("chart", chart.Builtin(watchResultsProvider))
//	core.RegisterPipeStage("chart", chart.ParsePipeArgs, chart.RenderPipe)
//
// Or, equivalently, the convenience entry point:
//
//	chart.Register(core, watchResultsProvider)
//
// The package depends only on warp/pkg/shellcore (for the View
// interface and Core methods) and warp/pkg/shellcore/chart (for the
// generic chart-rendering primitives). Weft-specific knowledge
// (WatchResultsProvider, RFC3339 timestamps, /cron.d/ paths) lives
// here.
package chart

import "time"

// WatchResultValue is one cron result returned by WatchResultsProvider.
// Value is the watch's stdout (trimmed); RanAt is RFC3339.
type WatchResultValue struct {
	Value string // stdout, trimmed
	RanAt string // RFC3339
}

// WatchResultsProvider can fetch recent results for a watch. The
// embedder's FS implements this so the chart View can pull data
// without coupling shellcore to weft's storage layer.
type WatchResultsProvider interface {
	GetWatchResults(name string, limit int) ([]WatchResultValue, error)
}

// WatchArgs holds a parsed `chart <name> [...]` invocation
// (the watch form — distinct from PipeArgs which parses the
// `... | chart` form).
type WatchArgs struct {
	Name     string
	Kind     string        // "line", "sparkline", "bar"; "" = auto
	Limit    int           // results to fetch (default 50)
	Interval time.Duration // poll interval in --follow mode (default 1s)
	Follow   bool
}
