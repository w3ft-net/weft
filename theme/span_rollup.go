package theme

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SpanOccurrence is one "span done" event rolled into a SpanRollupRow —
// the per-instance detail the interactive drill-down's "samples" level
// shows (which label/shard it hit, its own duration). TraceID is what
// lets the samples level hand off into a `trace <id>` view for a
// specific request — the same field's value is repeated across every
// occurrence within one Phase 1 (single-trace) rollup, but varies per
// row in Phase 2's cross-trace aggregation.
//
// Duration is inclusive (wall-clock from this span's own start to its
// own end, including everything its children did — standard tracing
// semantics, same as every span in Jaeger/Zipkin/OTel). SelfDuration is
// Duration minus the time covered by this span's children (matched via
// ParentSpanID, within the same line set) — the number that actually
// answers "where is the bottleneck," since an inclusive duration high
// up a call chain mostly reflects delegated work, not its own. It's
// computed correctly for overlapping/concurrent children (a union of
// their time ranges, not a naive sum — see computeSelfDurations) but
// is only as complete as the line set it was computed from: it's
// accurate for a single full trace (Phase 1, `trace <id>`), but
// understates delegation for Phase 2's cross-trace `trace spans`
// queries, which only fetch rows matching one span name and so can't
// see cross-name children at all. Callers gate use of it (rolling it
// into SpanRollupRow.SelfTotal) on that distinction (see
// SpanRollupColumns' singleTrace) rather than showing a self-time
// number that looks complete but isn't.
type SpanOccurrence struct {
	Label        string // the row's "namespace/service" column
	TraceID      string
	SpanID       string
	ParentSpanID string
	Duration     time.Duration
	SelfDuration time.Duration
	Status       string // "ok" or "error", from the span's own `status=` field
}

// SpanRollupRow is one distinct span name's aggregate stats within the
// line set RenderSpanRollup was given: count, error count,
// min/avg/p50/p99/max inclusive duration across every "span done"
// event for that name (meaningful once there are enough occurrences
// across enough traces to form a distribution — Phase 2's cross-trace
// aggregation), and SelfTotal, the sum of self durations (see
// SpanOccurrence) across this name's occurrences (meaningful for a
// single trace, where percentiles over 1-3 samples aren't, but "how
// much of this one request's time did this span name account for" is
// — see SpanRollupColumns' singleTrace). Zero value (no occurrences)
// leaves the duration fields at 0 — callers check Count() before
// reading them.
type SpanRollupRow struct {
	Name        string
	Label       string
	Calls       int
	Occurrences []SpanOccurrence
	Errors      int
	// NoLevel is set when no row behind this name carried a log level, so
	// whether any span errored is unknown rather than zero. The zero value
	// (false) is the ordinary case: levels were recorded.
	NoLevel                 bool
	Total                   time.Duration
	Min, Avg, P50, P99, Max time.Duration
	SelfTotal               time.Duration
}

// displayName is the span name, with its service when rows are grouped by it.
func (r SpanRollupRow) displayName() string {
	if r.Label == "" {
		return r.Name
	}
	return r.Name + " (" + r.Label + ")"
}

// Count reports how many "span done" events this row aggregates.
func (r SpanRollupRow) Count() int { return r.Calls }

// parsedOcc is a "span done" event as parsed, before self-time and
// before grouping by name — the flat, cross-name shape
// computeSelfDurations needs (a child's name is irrelevant to finding
// it by ParentSpanID, so this intentionally isn't SpanOccurrence, which
// is already grouped under one name by the time callers see it).
type parsedOcc struct {
	name         string
	label        string
	traceID      string
	spanID       string
	parentSpanID string
	startNs      int64
	endNs        int64
	duration     time.Duration
	status       string
}

// SpanSamplesLimit is how many occurrences `trace spans <name> --samples`
// shows; the planner asks each shard for the same number.
const SpanSamplesLimit = 50

// spanGroupKey identifies one rollup row of a single trace: the same span
// name in two services is two different things, so the service is part of it.
type spanGroupKey struct{ label, name string }

type spanOccurrenceJSON struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	TraceID   string `json:"trace_id"`
	SpanID    string `json:"span_id"`
	ParentID  string `json:"parent_id"`
	DurNs     int64  `json:"dur_ns"`
	TsNs      int64  `json:"ts_ns"`
	Level     string `json:"level"`
}

func (o spanOccurrenceJSON) label() string {
	if o.Namespace == "" && o.Service == "" {
		return "unknown"
	}
	return o.Namespace + "/" + o.Service
}

func parseSpanOccurrences(lines []string) []spanOccurrenceJSON {
	var out []spanOccurrenceJSON
	for _, l := range lines {
		var o spanOccurrenceJSON
		if json.Unmarshal([]byte(l), &o) != nil || o.Name == "" {
			continue
		}
		out = append(out, o)
	}
	return out
}

// RenderSpanRollup rolls the span rows of one trace (the JSON lines of
// `trace <id> --spans`) up into one SpanRollupRow per span name. A row with a
// duration is a finished span; a row at level ERROR is a span error, counted
// against its name and marking its span as failed. Self time comes from the
// parent and child relations among these rows, so it is only as complete as
// the trace the rows came from.
func RenderSpanRollup(lines []string) []SpanRollupRow {
	var occs []parsedOcc
	errorsByName := make(map[spanGroupKey]int)
	leveled := make(map[spanGroupKey]bool)
	failedSpans := make(map[string]bool)

	for _, o := range parseSpanOccurrences(lines) {
		key := spanGroupKey{o.label(), o.Name}
		if o.Level != "" {
			leveled[key] = true
		}
		if o.DurNs > 0 {
			var startNs int64
			if o.TsNs > 0 {
				startNs = o.TsNs - o.DurNs
			}
			occs = append(occs, parsedOcc{
				name: o.Name, label: o.label(), traceID: o.TraceID, spanID: o.SpanID, parentSpanID: o.ParentID,
				startNs: startNs, endNs: o.TsNs, duration: time.Duration(o.DurNs), status: "ok",
			})
			continue
		}
		if strings.EqualFold(o.Level, "ERROR") {
			errorsByName[key]++
			failedSpans[o.SpanID] = true
		}
	}
	for i := range occs {
		if failedSpans[occs[i].spanID] {
			occs[i].status = "error"
		}
	}

	selfDurations := computeSelfDurations(occs)

	byKey := make(map[spanGroupKey][]SpanOccurrence)
	seen := make(map[spanGroupKey]bool)
	var order []spanGroupKey
	addKey := func(k spanGroupKey) {
		if !seen[k] {
			seen[k] = true
			order = append(order, k)
		}
	}
	for i, o := range occs {
		k := spanGroupKey{o.label, o.name}
		addKey(k)
		byKey[k] = append(byKey[k], SpanOccurrence{
			Label: o.label, TraceID: o.traceID, SpanID: o.spanID, ParentSpanID: o.parentSpanID,
			Duration: o.duration, SelfDuration: selfDurations[i], Status: o.status,
		})
	}
	for k := range errorsByName {
		addKey(k)
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i].name != order[j].name {
			return order[i].name < order[j].name
		}
		return order[i].label < order[j].label
	})
	rows := make([]SpanRollupRow, 0, len(order))
	for _, k := range order {
		occ := byKey[k]
		durs := make([]time.Duration, len(occ))
		var selfTotal time.Duration
		for i, o := range occ {
			durs[i] = o.Duration
			selfTotal += o.SelfDuration
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		row := SpanRollupRow{
			Name: k.name, Label: k.label, Calls: len(occ), Occurrences: occ,
			Errors: errorsByName[k], NoLevel: !leveled[k], SelfTotal: selfTotal,
		}
		if len(durs) > 0 {
			row.Total = sumDuration(durs)
			row.Min = durs[0]
			row.Max = durs[len(durs)-1]
			row.Avg = row.Total / time.Duration(len(durs))
			row.P50 = percentileDuration(durs, 0.50)
			row.P99 = percentileDuration(durs, 0.99)
		}
		rows = append(rows, row)
	}
	return rows
}

type spanAggregateJSON struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Service   string `json:"service"`
	Count     int    `json:"count"`
	Errors    *int   `json:"errors"` // null when no row carried a level
	TotalNs   int64  `json:"total_ns"`
	AvgNs     int64  `json:"avg_ns"`
	P50Ns     int64  `json:"p50_ns"`
	P99Ns     int64  `json:"p99_ns"`
	MinNs     int64  `json:"min_ns"`
	MaxNs     int64  `json:"max_ns"`
}

// RenderSpanAggregate reads the JSON lines of `trace spans [name]` (one per
// span name, or per name and service) into rollup rows. The percentiles are
// approximate, as computed by the shards.
func RenderSpanAggregate(lines []string) []SpanRollupRow {
	var rows []SpanRollupRow
	for _, l := range lines {
		var a spanAggregateJSON
		if json.Unmarshal([]byte(l), &a) != nil || a.Name == "" {
			continue
		}
		row := SpanRollupRow{
			Name: a.Name, Calls: a.Count, NoLevel: a.Errors == nil,
			Total: time.Duration(a.TotalNs), Avg: time.Duration(a.AvgNs),
			P50: time.Duration(a.P50Ns), P99: time.Duration(a.P99Ns),
			Min: time.Duration(a.MinNs), Max: time.Duration(a.MaxNs),
		}
		if a.Errors != nil {
			row.Errors = *a.Errors
		}
		if a.Namespace != "" || a.Service != "" {
			row.Label = a.Namespace + "/" + a.Service
		}
		rows = append(rows, row)
	}
	return rows
}

// RenderSpanSamples reads the JSON lines of `trace spans <name> --samples`
// into occurrences, slowest first.
func RenderSpanSamples(lines []string, limit int) []SpanOccurrence {
	var occ []SpanOccurrence
	for _, o := range parseSpanOccurrences(lines) {
		status := "ok"
		if strings.EqualFold(o.Level, "ERROR") {
			status = "error"
		}
		occ = append(occ, SpanOccurrence{
			Label: o.label(), TraceID: o.TraceID, SpanID: o.SpanID, ParentSpanID: o.ParentID,
			Duration: time.Duration(o.DurNs), SelfDuration: time.Duration(o.DurNs), Status: status,
		})
	}
	sort.SliceStable(occ, func(i, j int) bool { return occ[i].Duration > occ[j].Duration })
	if limit > 0 && len(occ) > limit {
		occ = occ[:limit]
	}
	return occ
}

// computeSelfDurations returns, parallel to occs, each occurrence's
// self (exclusive) duration: its own Duration minus the time covered
// by its children (occurrences whose parentSpanID matches its spanID),
// clamped to >= 0 to absorb clock/logging skew that could otherwise
// push it slightly negative. A span with no known children (a leaf, or
// one whose children weren't in this line set at all — see
// SpanOccurrence's doc comment on completeness) gets self == inclusive.
//
// Children are combined via interval union, not naive summation: this
// mirrors what actual production self-time implementations do (OTel's
// spanmetrics connector, New Relic's exclusive-duration calculation —
// both explicitly merge overlapping child intervals before subtracting)
// and matters concretely here, since weft's own span shape fans a
// parent span (e.g. coordinator.Execute) out to multiple *concurrent*
// children (e.g. several shard.Query calls) — summing their durations
// naively would double-subtract the overlapping time and could easily
// drive self-time negative for exactly the fan-out spans this feature
// exists to make visible.
func computeSelfDurations(occs []parsedOcc) []time.Duration {
	childrenOf := make(map[string][]int) // parentSpanID -> indices into occs
	for i, o := range occs {
		if o.parentSpanID != "" {
			childrenOf[o.parentSpanID] = append(childrenOf[o.parentSpanID], i)
		}
	}

	self := make([]time.Duration, len(occs))
	for i, o := range occs {
		children := childrenOf[o.spanID]
		if len(children) == 0 || (o.startNs == 0 && o.endNs == 0) {
			self[i] = o.duration
			continue
		}
		intervals := make([][2]int64, 0, len(children))
		for _, ci := range children {
			c := occs[ci]
			if c.startNs == 0 && c.endNs == 0 {
				continue // unusable timestamp; can't place this child in time
			}
			// Clip to the parent's own span — a child's logged
			// timestamp can extend a hair past the parent's own due to
			// ordinary clock/logging skew, and an uncapped interval
			// would make covered() exceed the parent's own duration.
			start, end := c.startNs, c.endNs
			if start < o.startNs {
				start = o.startNs
			}
			if end > o.endNs {
				end = o.endNs
			}
			if end > start {
				intervals = append(intervals, [2]int64{start, end})
			}
		}
		covered := coveredNs(intervals)
		selfNs := o.duration.Nanoseconds() - covered
		if selfNs < 0 {
			selfNs = 0
		}
		self[i] = time.Duration(selfNs)
	}
	return self
}

// coveredNs returns the total duration covered by intervals (each a
// half-open [start,end) nanosecond range), merging overlaps so time
// covered by more than one interval is counted once.
func coveredNs(intervals [][2]int64) int64 {
	if len(intervals) == 0 {
		return 0
	}
	sort.Slice(intervals, func(i, j int) bool { return intervals[i][0] < intervals[j][0] })
	var total int64
	curStart, curEnd := intervals[0][0], intervals[0][1]
	for _, iv := range intervals[1:] {
		if iv[0] > curEnd {
			total += curEnd - curStart
			curStart, curEnd = iv[0], iv[1]
			continue
		}
		if iv[1] > curEnd {
			curEnd = iv[1]
		}
	}
	total += curEnd - curStart
	return total
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
// but NAME holds a short duration/count/bar string, so only NAME needs
// to flex with termWidth.
const (
	spanBarWidth   = 10
	spanCountWidth = 6
	spanDurWidth   = 9
	spanErrWidth   = 6
	spanNameMin    = 12
	spanColSpacing = 1
)

// FgReset ends a color span without canceling reverse video or an
// explicit background: unlike "\x1b[0m" (a full SGR reset, which also
// clears any "7m" reverse or "48;5;N" background attribute a caller
// may have wrapped the whole line in), "39" resets only the foreground
// color, "22" only bold/faint intensity, and "23" only italic — SGR
// attributes are independent parameters, so background/reverse
// survive. All three are needed: Key/Operator/Error bundle bold into
// their color escape (e.g. Palette().Key is "\x1b[1;38;5;214m") and
// Comment bundles italic, so resetting fg alone would leave later
// cells in the row bold or italic too. Exported so other packages that
// color per-cell text meant to sit inside a caller's own
// background/reverse wrap (spanview's occurrenceColumns, for instance)
// can reuse the exact same safe reset instead of composing their own.
const FgReset = "\x1b[39;22;23m"

// PadVisible right-pads (or truncates with a trailing "…") s to
// exactly w VISIBLE characters, counting runes rather than bytes.
// Byte length over-counts any multi-byte rune — time.Duration.String()
// itself is the concrete case this exists for: "669.006µs" is 9
// visible characters but 10 bytes (µ is 2 bytes in UTF-8), so a
// byte-length check against a 9-wide column would wrongly decide it
// needs truncating and silently drop a real character. Exported so
// spanview's occurrenceColumns (a separate package, formatting the
// same duration values into fixed-width columns) can share it instead
// of re-deriving the same fix.
func PadVisible(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		if w > 1 {
			return string(r[:w-1]) + "…"
		}
		return string(r[:w])
	}
	return s + strings.Repeat(" ", w-len(r))
}

// PadLeftVisible left-pads (or right-truncates, keeping the tail) s to
// exactly w visible characters — see PadVisible.
func PadLeftVisible(s string, w int) string {
	r := []rune(s)
	if len(r) > w {
		return string(r[len(r)-w:])
	}
	return strings.Repeat(" ", w-len(r)) + s
}

// renderSpanBar draws a bar relative to the largest value in the set
// (inclusive average across span names for cross-trace mode, or total
// self time for single-trace mode — see SpanRollupColumns' singleTrace),
// sibling to trace_columns.go's renderGapBar: filled proportionally to
// value/maxValue (0, or a single row with nothing to compare against,
// renders as all spaces rather than a misleading sliver). Colored in
// the palette's Operator orange — the "weft" (warm) half of the
// theme's weave, distinct from the muted gray renderGapBar uses for
// the trace view's gap histogram, so a span-name comparison reads as a
// different kind of bar than "time since the last log line." Ends
// with FgReset, not a full reset, so it composes under spanview's
// reverse-video focus wrap.
func renderSpanBar(value, maxValue time.Duration, width int) string {
	n := 0
	if maxValue > 0 {
		n = int(float64(value) / float64(maxValue) * float64(width))
		if n > width {
			n = width
		}
	}
	return Palette().Operator + strings.Repeat(" ", width-n) + strings.Repeat("█", n) + FgReset
}

// SpanRollupColumns lays rows out into fixed-width text: a header line
// and one line per row. The column set depends on singleTrace, which
// callers derive from whether the line set they queried is a complete
// single trace (see isSpansAggregatePipeline in shell/shell.go and
// spanview/view.go):
//
//   - singleTrace=true (`trace <id> --spans`, Phase 1): "NAME BAR COUNT
//     TOTAL ERRORS". Percentiles/avg/min/max over 1-3 occurrences of a
//     span name within one trace aren't a meaningful distribution —
//     TOTAL (the sum of self/exclusive durations, see SpanOccurrence)
//     answers the question that actually matters for one request: how
//     much of its elapsed time did this span name account for, in its
//     own right (not delegated to children). BAR scales against the
//     largest TOTAL, so it points at the real per-request bottleneck.
//   - singleTrace=false (`trace spans [name]`, Phase 2): "NAME BAR COUNT
//     AVG P50 P99 MIN MAX ERRORS" — the full inclusive-duration
//     distribution, which self-time can't be computed for anyway (Phase
//     2 only fetches rows matching one span name, so it can never see
//     cross-name children — see SpanOccurrence's doc comment). BAR
//     scales against the largest average.
//
// BAR's header cell is always blank — the bar graphic itself doesn't
// need a text label to be legible as "a bar," and "BAR" was just
// visual noise next to it.
//
// Data rows are colored (NAME in the palette's Key gold, durations in
// Number indigo, ERRORS in Error salmon when nonzero, BAR per
// renderSpanBar) using FgReset rather than a full SGR reset between
// cells, so spanview's interactive list can safely wrap a focused row
// in reverse video ("\x1b[7m") without an embedded color reset
// canceling it early — reverse video and foreground color are
// independent SGR attributes. The header stays plain label text;
// callers (RenderSpanRollupTable, spanview) apply their own header
// treatment. Exported so spanview (a separate package — it also pulls
// in shellcore, which span_rollup.go's callers here shouldn't need)
// can reuse it exactly.
func SpanRollupColumns(rows []SpanRollupRow, termWidth int, singleTrace bool) (header string, rowTexts []string) {
	// singleTrace: 5 fields (name, bar, count, total, errs) need 4
	// separators and 1 duration-width column (TOTAL). Otherwise: 9
	// fields (name, bar, count, avg, p50, p99, min, max, errs) need 8
	// separators and 5 duration-width columns. An earlier version of
	// this budget undercounted the non-singleTrace case (4*spanDurWidth,
	// 7*spanColSpacing), leaving nameWidth 10 chars too wide and every
	// row 10 chars past termWidth — which a redrawing interactive view
	// (spanview) turns into visible overflow/wrap, not just a cosmetic
	// width mismatch.
	durCols, seps := 5, 8
	if singleTrace {
		durCols, seps = 1, 4
	}
	nameWidth := termWidth - spanBarWidth - spanCountWidth - durCols*spanDurWidth - spanErrWidth - seps*spanColSpacing
	if nameWidth < spanNameMin {
		nameWidth = spanNameMin
	}

	var maxAvg, maxTotal time.Duration
	for _, r := range rows {
		if r.Avg > maxAvg {
			maxAvg = r.Avg
		}
		if r.SelfTotal > maxTotal {
			maxTotal = r.SelfTotal
		}
	}
	barAgainst := maxAvg
	if singleTrace {
		barAgainst = maxTotal
	}

	pad, padLeft := PadVisible, PadLeftVisible
	color := func(s, sgr string) string { return sgr + s + FgReset }
	sep := strings.Repeat(" ", spanColSpacing)
	blankBar := strings.Repeat(" ", spanBarWidth)

	headerCells := []string{pad("NAME", nameWidth), blankBar, padLeft("COUNT", spanCountWidth)}
	if singleTrace {
		headerCells = append(headerCells, padLeft("TOTAL", spanDurWidth))
	} else {
		headerCells = append(headerCells,
			padLeft("AVG", spanDurWidth), padLeft("P50", spanDurWidth), padLeft("P99", spanDurWidth),
			padLeft("MIN", spanDurWidth), padLeft("MAX", spanDurWidth),
		)
	}
	headerCells = append(headerCells, padLeft("ERRORS", spanErrWidth))
	header = strings.Join(headerCells, sep)

	rowTexts = make([]string, len(rows))
	for i, r := range rows {
		errs := padLeft(strconv.Itoa(r.Errors), spanErrWidth)
		if r.NoLevel {
			errs = padLeft("-", spanErrWidth)
		} else if r.Errors > 0 {
			errs = color(errs, Palette().Error)
		}
		barValue := r.Avg
		if singleTrace {
			barValue = r.SelfTotal
		}
		cells := []string{
			color(pad(r.displayName(), nameWidth), Palette().Key),
			renderSpanBar(barValue, barAgainst, spanBarWidth),
			color(padLeft(strconv.Itoa(r.Count()), spanCountWidth), Palette().Number),
		}
		if singleTrace {
			cells = append(cells, color(padLeft(r.SelfTotal.String(), spanDurWidth), Palette().Number))
		} else {
			cells = append(cells,
				color(padLeft(r.Avg.String(), spanDurWidth), Palette().Number),
				color(padLeft(r.P50.String(), spanDurWidth), Palette().Number),
				color(padLeft(r.P99.String(), spanDurWidth), Palette().Number),
				color(padLeft(r.Min.String(), spanDurWidth), Palette().Number),
				color(padLeft(r.Max.String(), spanDurWidth), Palette().Number),
			)
		}
		cells = append(cells, errs)
		rowTexts[i] = strings.Join(cells, sep)
	}
	return header, rowTexts
}

// RenderSpanRollupTable renders rows into a ready-to-print ANSI table —
// header in the theme's Key color, data rows colored per
// SpanRollupColumns — sized to termWidth. singleTrace is passed
// straight through to SpanRollupColumns (see its doc comment on the
// two column sets). Empty rows renders a single dimmed "no spans" line
// so callers (ExecNonInteractive's `trace <id> --spans`) can print the
// result unconditionally.
func RenderSpanRollupTable(rows []SpanRollupRow, termWidth int, singleTrace bool) []string {
	if len(rows) == 0 {
		return []string{Palette().Comment + "(no spans found in this trace)" + "\x1b[0m"}
	}
	header, rowTexts := SpanRollupColumns(rows, termWidth, singleTrace)
	out := make([]string, 0, len(rowTexts)+1)
	out = append(out, Palette().Key+header+"\x1b[0m")
	out = append(out, rowTexts...)
	return out
}
