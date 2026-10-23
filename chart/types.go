// Package chart implements weft's `chart` builtin and pipe-stage,
// plus the live-redraw View triggered by `chart <cron> --follow`.
//
// It registers into shellcore via Register at construction time:
//
//	cv := chart.NewView(src, rangeSpec)
//	core.RegisterView(cv)
//	core.RegisterBuiltin("chart", chart.Builtin(src, rangeSpec))
//	core.RegisterPipeStage("chart", chart.ParsePipeArgs, chart.RenderPipe(rangeSpec))
//
// Or, equivalently, the convenience entry point:
//
//	chart.Register(core, src, rangeSpec)
//
// The package depends only on warp/pkg/shellcore (for the View
// interface and Core methods) and warp/pkg/shellcore/chart (for the
// generic chart-rendering primitives). Weft-specific knowledge
// (CronSource, /cron.d/ paths) lives here.
package chart

import "time"

// CronBucket is one stored point of a cron: a raw run, or a rollup bucket
// holding the exact count/sum/min/max of the runs in it.
type CronBucket struct {
	Start time.Time
	Tag   string
	Count int64
	Sum   float64
	Min   float64
	Max   float64
}

// CronSeries is a cron's stored points over a window, from one storage
// tier. Granularity is "raw", "hourly", "4h" or "daily"; Start and End are
// the window after the server resolved the range and clamped it to the
// plan's retention.
type CronSeries struct {
	Granularity string
	Start, End  time.Time
	Buckets     []CronBucket // oldest first
}

// CronSource fetches a cron's stored series for a range spec ("2h..",
// "2026-04-01..2026-04-12"). The server resolves the spec against its own
// clock. The embedder's FS implements this so the chart builtin and View
// can pull data without coupling shellcore to weft's storage layer.
type CronSource interface {
	GetCronSeries(name, rangeSpec string) (CronSeries, error)
}

// WatchResultValue is one recent run of a cron as stored: its stdout
// (trimmed) and RanAt (RFC3339). Dashboard table tiles show the latest.
type WatchResultValue struct {
	Value string
	RanAt string
}

// WatchArgs holds a parsed `chart <cron> [...]` invocation (the cron
// form — distinct from PipeArgs which parses the `... | chart` form).
type WatchArgs struct {
	Name      string
	Kind      string        // "line", "sparkline", "bar"
	Aggregate string        // count | sum | avg | min | max | p50 | p95
	Bucket    time.Duration // 0 = the stored points as they are
	Title     string
	Interval  time.Duration // poll interval in --follow mode (default 1s)
	Follow    bool
}
