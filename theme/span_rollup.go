package theme

import (
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// spanDoneRe/spanErrorRe match the two fixed log.Printf shapes weft-go's
// Span type emits (span.go's End/RecordError) verbatim — no JSON, so a
// single regex per shape is enough. "span start" lines carry nothing a
// rollup needs beyond what "span done" repeats, so there's no pattern
// for it here.
var (
	spanDoneRe  = regexp.MustCompile(`^span done name=(\S+) trace_id=(\S+) span_id=(\S+) parent_span_id=(\S*) duration=\S+ duration_ns=(\d+) status=(\S+)$`)
	spanErrorRe = regexp.MustCompile(`^span error name=(\S+) trace_id=(\S+) span_id=(\S+) err=`)
)

// SpanOccurrence is one "span done" event rolled into a SpanRollupRow —
// the per-instance detail the interactive drill-down's "samples" level
// shows (which label/shard it hit, its own duration). TraceID is what
// lets the samples level hand off into a `trace <id>` view for a
// specific request — the same field's value is repeated across every
// occurrence within one Phase 1 (single-trace) rollup, but varies per
// row in Phase 2's cross-trace aggregation.
type SpanOccurrence struct {
	Label    string // the row's "namespace/service" column
	TraceID  string
	SpanID   string
	Duration time.Duration
	Status   string // "ok" or "error", from the span's own `status=` field
}

// SpanRollupRow is one distinct span name's aggregate stats within the
// line set RenderSpanRollup was given: count, error count, and
// min/avg/p50/p99/max duration across every "span done" event for that
// name. Zero value (no occurrences) leaves the duration fields at 0 —
// callers check Count() before reading them.
type SpanRollupRow struct {
	Name                    string
	Occurrences             []SpanOccurrence
	Errors                  int
	Min, Avg, P50, P99, Max time.Duration
}

// Count reports how many "span done" events this row aggregates.
func (r SpanRollupRow) Count() int { return len(r.Occurrences) }

// RenderSpanRollup parses lines — the same "<ts_unix_ns>\t<namespace/
// service>\t<raw message>" shape compute.LocalComputeJob's
// emitTraceTagged emits, and RenderTraceColumns already consumes — for
// "span done"/"span error" lines, grouping them into one SpanRollupRow
// per distinct span name. Every other line (ordinary log lines, the "#
// N line(s)..." summary, malformed rows) is silently skipped: this is a
// pure aggregation pass over data already fetched for a `trace <id>`
// query, not a renderer, so there's nothing to pass through. Rows are
// sorted by name for stable output.
func RenderSpanRollup(lines []string) []SpanRollupRow {
	type acc struct {
		occ    []SpanOccurrence
		errors int
	}
	byName := make(map[string]*acc)
	var order []string
	get := func(name string) *acc {
		a, ok := byName[name]
		if !ok {
			a = &acc{}
			byName[name] = a
			order = append(order, name)
		}
		return a
	}

	for _, l := range lines {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		label, raw := parts[1], parts[2]

		if m := spanDoneRe.FindStringSubmatch(raw); m != nil {
			ns, err := strconv.ParseInt(m[5], 10, 64)
			if err != nil {
				continue
			}
			a := get(m[1])
			a.occ = append(a.occ, SpanOccurrence{Label: label, TraceID: m[2], SpanID: m[3], Duration: time.Duration(ns), Status: m[6]})
			continue
		}
		if m := spanErrorRe.FindStringSubmatch(raw); m != nil {
			get(m[1]).errors++
		}
	}

	sort.Strings(order)
	rows := make([]SpanRollupRow, 0, len(order))
	for _, name := range order {
		a := byName[name]
		durs := make([]time.Duration, len(a.occ))
		for i, o := range a.occ {
			durs[i] = o.Duration
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		row := SpanRollupRow{Name: name, Occurrences: a.occ, Errors: a.errors}
		if len(durs) > 0 {
			row.Min = durs[0]
			row.Max = durs[len(durs)-1]
			row.Avg = sumDuration(durs) / time.Duration(len(durs))
			row.P50 = percentileDuration(durs, 0.50)
			row.P99 = percentileDuration(durs, 0.99)
		}
		rows = append(rows, row)
	}
	return rows
}

func sumDuration(durs []time.Duration) time.Duration {
	var sum time.Duration
	for _, d := range durs {
		sum += d
	}
	return sum
}

// percentileDuration returns the nearest-rank p-th percentile (p in
// [0,1]) of sorted, which must already be sorted ascending and
// non-empty — the same exact, no-interpolation method the "Aggregation
// mechanics" section of docs/plans/span-display.md settled on for
// cross-trace stats, applied here at one-trace scale.
func percentileDuration(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	idx := int(math.Ceil(p*float64(n))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= n {
		idx = n - 1
	}
	return sorted[idx]
}

// Column widths for spanRollupColumns' fixed-width layout. Every column
// but NAME holds a short duration/count string, so only NAME needs to
// flex with termWidth.
const (
	spanCountWidth = 6
	spanDurWidth   = 9
	spanErrWidth   = 6
	spanNameMin    = 12
	spanColSpacing = 1
)

// SpanRollupColumns lays rows out into plain (uncolored) fixed-width
// text: a header line and one line per row, "NAME COUNT AVG P50 P99 MIN
// MAX ERRORS". Uncolored deliberately — spanview's interactive list
// wraps a focused row in reverse video, which an embedded ANSI reset
// mid-line would prematurely cancel, so this is the single shared
// layout both spanview's plain interactive list and
// RenderSpanRollupTable's colored header build on. Exported so
// spanview (a separate package — it also pulls in shellcore, which
// span_rollup.go's callers here shouldn't need) can reuse it exactly.
func SpanRollupColumns(rows []SpanRollupRow, termWidth int) (header string, rowTexts []string) {
	nameWidth := termWidth - spanCountWidth - 4*spanDurWidth - spanErrWidth - 6*spanColSpacing
	if nameWidth < spanNameMin {
		nameWidth = spanNameMin
	}

	pad := func(s string, w int) string {
		if len(s) > w {
			if w > 1 {
				return s[:w-1] + "…"
			}
			return s[:w]
		}
		return s + strings.Repeat(" ", w-len(s))
	}
	padLeft := func(s string, w int) string {
		if len(s) > w {
			return s[len(s)-w:]
		}
		return strings.Repeat(" ", w-len(s)) + s
	}
	sep := strings.Repeat(" ", spanColSpacing)
	row := func(name, count, avg, p50, p99, min, max, errs string) string {
		return strings.Join([]string{
			pad(name, nameWidth),
			padLeft(count, spanCountWidth),
			padLeft(avg, spanDurWidth),
			padLeft(p50, spanDurWidth),
			padLeft(p99, spanDurWidth),
			padLeft(min, spanDurWidth),
			padLeft(max, spanDurWidth),
			padLeft(errs, spanErrWidth),
		}, sep)
	}

	header = row("NAME", "COUNT", "AVG", "P50", "P99", "MIN", "MAX", "ERRORS")
	rowTexts = make([]string, len(rows))
	for i, r := range rows {
		rowTexts[i] = row(r.Name, strconv.Itoa(r.Count()),
			r.Avg.String(), r.P50.String(), r.P99.String(), r.Min.String(), r.Max.String(),
			strconv.Itoa(r.Errors))
	}
	return header, rowTexts
}

// RenderSpanRollupTable renders rows into a ready-to-print ANSI table —
// header in the theme's Key color, plain rows below — sized to
// termWidth. Empty rows renders a single dimmed "no spans" line so
// callers (ExecNonInteractive's `trace <id> --spans`) can print the
// result unconditionally.
func RenderSpanRollupTable(rows []SpanRollupRow, termWidth int) []string {
	if len(rows) == 0 {
		return []string{Palette().Comment + "(no spans found in this trace)" + "\x1b[0m"}
	}
	header, rowTexts := SpanRollupColumns(rows, termWidth)
	out := make([]string, 0, len(rowTexts)+1)
	out = append(out, Palette().Key+header+"\x1b[0m")
	out = append(out, rowTexts...)
	return out
}
