package chart

import (
	"fmt"
	"github.com/w3ft-net/weft/fieldspec"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/w3ft-net/weft/timerange"
)

// Spec declares the `chart` command line. One command, two forms: the flags
// are the union, and each form rejects the other's (see WatchArgsFrom and
// PipeArgsFrom). The window is the shell's range, not a flag.
var Spec = bash.Spec{
	Name:    "chart",
	Summary: "chart a cron's results, or the output of the pipeline before it",
	Description: "Alone, `chart <cron>` charts a cron's saved results; -f redraws it live.\n" +
		"After a pipe, `... | chart -x 1 -y 2` charts the upstream rows.\n" +
		"The time span is the shell's range.",
	Flags: []bash.Flag{
		bash.String("aggregate", 'a', "reduction: count, sum, avg, min, max, p50, p95 (pipe form also identity)").Arg("FN"),
		bash.String("bucket", 0, "group rows into time buckets of DUR (e.g. 1m, 1h)").Arg("DUR"),
		bash.String("kind", 0, "chart type").Arg("KIND").Choices("line", "sparkline", "bar"),
		bash.String("title", 0, "chart title").Arg("TEXT"),
		bash.Bool("follow", 'f', "watch form: redraw live"),
		bash.String("interval", 'i', "watch form, with -f: redraw every DUR (min 100ms)").Arg("DUR"),
		bash.String("x", 'x', "pipe form: x column or .json.path, optionally with a ::time cast").Arg("COL").ShortOnly(),
		bash.String("y", 'y', "pipe form: y column or .json.path").Arg("COL").ShortOnly(),
		bash.String("tag", 't', "pipe form: one line per value of this column or .json.path").Arg("COL"),
	},
	Args:     bash.Operands("CRON").Any().Complete(bash.NoCompletion),
	Strategy: bash.StageBarrier,
}

// chartUsage is the one-line usage of the watch form, shown when no cron is named.
const chartUsage = "usage: chart <cron> [-a FN] [--bucket DUR] [--kind KIND] [--title TEXT] [-f [-i DUR]]"

// parseSpec runs Spec over args and turns a usage error into the terse
// message the shell shows.
func parseSpec(args []string) (p *bash.Parsed, msg string) {
	p, err := Spec.Parse(args)
	if err != nil {
		return nil, err.Error()
	}
	return p, ""
}

// ParseWatchCmd parses a tokenised `chart …` command line into
// WatchArgs. The first non-flag token is the cron name (bare or
// `/cron.d/<name>`). Returns an error message when the user types
// something we don't grok — the message is what shows up in the
// shell, so it should be terse. The window is the shell's range, not
// a flag.
func ParseWatchCmd(args []string) (WatchArgs, string) {
	p, msg := parseSpec(args)
	if msg != "" {
		return WatchArgs{Interval: time.Second, Aggregate: "avg"}, msg
	}
	return WatchArgsFrom(p)
}

// WatchArgsFrom reads the watch form out of a parsed `chart` line.
func WatchArgsFrom(p *bash.Parsed) (WatchArgs, string) {
	out := WatchArgs{Interval: time.Second, Aggregate: "avg"}
	if p.Help {
		return out, strings.TrimRight(Spec.Help(), "\n")
	}
	for _, f := range []string{"x", "y", "tag"} {
		if p.Has(f) {
			return out, fmt.Sprintf("chart: -%s is only for the pipe form (`... | chart`)", flagLabel(f))
		}
	}
	out.Kind = p.String("kind")
	if p.Has("aggregate") {
		out.Aggregate = p.String("aggregate")
		if !ValidCronAggregate(out.Aggregate) {
			return out, fmt.Sprintf("chart: unknown --aggregate %q", out.Aggregate)
		}
	}
	if p.Has("bucket") {
		d, err := time.ParseDuration(p.String("bucket"))
		if err != nil || d <= 0 {
			return out, fmt.Sprintf("chart: --bucket: invalid duration %q", p.String("bucket"))
		}
		out.Bucket = d
	}
	out.Title = p.String("title")
	if p.Has("interval") {
		d, err := time.ParseDuration(p.String("interval"))
		if err != nil || d < 100*time.Millisecond {
			return out, "chart: -i must be a duration ≥ 100ms"
		}
		out.Interval = d
	}
	out.Follow = p.Bool("follow")
	ops := p.Operands()
	if len(ops) > 1 {
		return out, fmt.Sprintf("chart: unexpected argument %q (only one cron name allowed)", ops[1])
	}
	if len(ops) == 0 {
		return out, chartUsage
	}
	out.Name = ops[0]
	// Strip /cron.d/ prefix so the user can `chart /cron.d/foo` or `chart foo`.
	out.Name = strings.TrimPrefix(out.Name, "/cron.d/")
	out.Name = strings.TrimPrefix(out.Name, "cron.d/")
	if out.Kind == "" {
		out.Kind = "line"
	}
	return out, ""
}

// flagLabel is how a flag is named in a message: x and y as -x and -y.
func flagLabel(name string) string {
	if len(name) == 1 {
		return name
	}
	return "-" + name
}

// PipeArgs holds the parsed flags for `... | chart` (pipe-stage
// form). Distinct from WatchArgs (watch form) — the two parsers
// don't overlap. Exported so non-interactive callers (e.g.
// weft sh -c "...") can carry it across the query/render boundary.
type PipeArgs struct {
	XSpec     fieldspec.FieldSpec
	XEnd      int    // last column of an `-x <start>-<end>` range, joined by spaces; 0 = no range
	XTime     bool   // -x carries a ::time cast
	XFormat   string // strftime format from ::time(<format>); "" = RFC3339 or epoch seconds
	YSpec     fieldspec.FieldSpec
	TagSpec   fieldspec.FieldSpec // splits output into one line per tag value; line/sparkline only
	Aggregate string              // count | sum | avg | min | max | p50 | p95 | identity
	Bucket    time.Duration       // 0 = no bucketing
	Kind      string              // line | bar | sparkline
	Title     string
	// RangeStart/RangeEnd are the shell's range, set by the caller (not
	// parsed). With a time x-axis the chart spans the whole range, and
	// zero-filled buckets reach both ends; zero means the data's own span.
	RangeStart, RangeEnd time.Time
}

// WithRange returns args with the shell's range resolved from spec; an
// unparseable spec leaves the data's own span.
func (a PipeArgs) WithRange(spec string) PipeArgs {
	if start, end, err := timerange.ParseRange(spec); err == nil {
		a.RangeStart, a.RangeEnd = start, end
	}
	return a
}

// span returns the x extent a time chart spans: the range when one is set,
// widened to any data outside it.
func (a PipeArgs) span(lo, hi float64) (float64, float64) {
	if a.RangeStart.IsZero() {
		return lo, hi
	}
	return min(lo, float64(a.RangeStart.Unix())), max(hi, float64(a.RangeEnd.Unix()))
}

// usesJSONLines reports whether stdin should be parsed as one JSON
// document per line. True when x, y, or tag is a JSON-path spec.
func (a PipeArgs) usesJSONLines() bool {
	return a.XSpec.IsJSON() || a.YSpec.IsJSON() || a.TagSpec.IsJSON()
}

// ParsePipeArgs parses flag tokens passed to chart in pipe mode.
// The watch form (`chart <watch>`) routes to ParseWatchCmd instead.
//
// Defaults:
//   - aggregate: "count" when --bucket is set OR -y is absent;
//     "identity" otherwise (one row in, one point out).
//   - kind: "line" when --bucket or --tag is set, "bar" otherwise.
//
// Validation:
//   - -x, -y, and --tag may not mix positional and JSON-path forms.
//   - aggregate must be a known reduction.
//   - --tag is only supported for --kind line (rejected with bar/sparkline).
//
// Signature matches shellcore.PipeStageParseFunc — this function
// is handled by the same `chart` command (see Command).
func ParsePipeArgs(args []string) (any, string) {
	p, msg := parseSpec(args)
	if msg != "" {
		return PipeArgs{}, msg
	}
	return PipeArgsFrom(p)
}

// PipeArgsFrom reads the pipe form out of a parsed `chart` line.
func PipeArgsFrom(p *bash.Parsed) (any, string) {
	out := PipeArgs{}
	if p.Help {
		return out, strings.TrimRight(Spec.Help(), "\n")
	}
	for _, f := range []string{"follow", "interval"} {
		if p.Has(f) {
			return out, fmt.Sprintf("chart: --%s is only for the watch form (`chart <cron>`)", f)
		}
	}
	if ops := p.Operands(); len(ops) > 0 {
		return out, fmt.Sprintf("chart: unexpected argument %q (use flags only in pipe mode)", ops[0])
	}
	if p.Has("x") {
		if msg := parseXArg(p.String("x"), &out); msg != "" {
			return out, "chart: -x: " + msg
		}
	}
	for _, f := range []struct{ name, label string }{{"y", "-y"}, {"tag", "--tag"}} {
		if !p.Has(f.name) {
			continue
		}
		v := p.String(f.name)
		if strings.Contains(v, "::") {
			return out, fmt.Sprintf("chart: %s: casts are only supported on -x", f.label)
		}
		s, err := fieldspec.ParseFieldSpec(v)
		if err != nil {
			return out, fmt.Sprintf("chart: %s: %s", f.label, err.Error())
		}
		if f.name == "y" {
			out.YSpec = s
		} else {
			out.TagSpec = s
		}
	}
	out.Aggregate = p.String("aggregate")
	if p.Has("bucket") {
		d, err := time.ParseDuration(p.String("bucket"))
		if err != nil || d <= 0 {
			return out, fmt.Sprintf("chart: --bucket: invalid duration %q", p.String("bucket"))
		}
		out.Bucket = d
	}
	out.Kind = p.String("kind")
	out.Title = p.String("title")

	// Mixing positional and JSON-path is rejected — the line parser
	// can't be in two modes at once.
	specs := []fieldspec.FieldSpec{}
	for _, s := range []fieldspec.FieldSpec{out.XSpec, out.YSpec, out.TagSpec} {
		if !s.IsZero() {
			specs = append(specs, s)
		}
	}
	if len(specs) > 1 {
		for _, s := range specs[1:] {
			if s.IsJSON() != specs[0].IsJSON() {
				return out, "chart: -x, -y, and --tag must all be positional or all .json.path"
			}
		}
	}

	// Default kind: line when bucketing time or tagging, bar otherwise.
	if out.Kind == "" {
		if out.Bucket > 0 || !out.TagSpec.IsZero() {
			out.Kind = "line"
		} else {
			out.Kind = "bar"
		}
	}
	switch out.Kind {
	case "line", "bar", "sparkline":
	default:
		return out, fmt.Sprintf("chart: unknown kind %q (want line|bar|sparkline)", out.Kind)
	}
	// --tag draws one line per tag value, so it only makes sense for a
	// line chart — bar's X-axis is already categorical (use -x for
	// that), and there's no multi-series sparkline primitive.
	if !out.TagSpec.IsZero() && out.Kind != "line" {
		return out, "chart: --tag is only supported for line charts (use -x <tag field> for a grouped bar chart)"
	}

	// Default aggregate. With --bucket on, count is the most useful
	// reduction (rows per bucket). Without it each row is plotted
	// directly — identity preserves "the number on this line" for
	// the `wc -l | chart` case, where forcing count to 1 would lose
	// the upstream's actual value.
	if out.Aggregate == "" {
		if out.Bucket > 0 {
			out.Aggregate = "count"
		} else {
			out.Aggregate = "identity"
		}
	}
	if !validAggregate(out.Aggregate) {
		return out, fmt.Sprintf("chart: unknown --aggregate %q", out.Aggregate)
	}

	return out, ""
}

// parseXArg parses -x's value into out: a field spec, optionally
// suffixed with a `::time` or `::time(<strftime format>)` cast. A
// time-cast column may be a range (`1-2`), whose columns are joined
// by a space before parsing — for stamps like `2026/10/13 15:04:05`
// that span two whitespace fields. Returns a terse message on error.
func parseXArg(v string, out *PipeArgs) string {
	base, cast, hasCast := strings.Cut(v, "::")
	if hasCast {
		switch {
		case cast == "time":
		case strings.HasPrefix(cast, "time(") && strings.HasSuffix(cast, ")"):
			out.XFormat = strings.TrimSuffix(strings.TrimPrefix(cast, "time("), ")")
			if out.XFormat == "" {
				return "empty ::time() format"
			}
		default:
			return fmt.Sprintf("unknown cast %q (want time or time(<format>))", cast)
		}
		out.XTime = true
	}
	if lo, hi, ok := strings.Cut(base, "-"); ok && isDigits(lo) && isDigits(hi) {
		if !out.XTime {
			return "a column range needs a ::time cast"
		}
		start, _ := strconv.Atoi(lo)
		end, _ := strconv.Atoi(hi)
		if end <= start {
			return "range end must be after its start"
		}
		out.XEnd = end
		base = lo
	}
	spec, err := fieldspec.ParseFieldSpec(base)
	if err != nil {
		return err.Error()
	}
	out.XSpec = spec
	return ""
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// ValidCronAggregate reports whether s is an aggregate `chart <cron>` takes:
// every pipe-form reduction except identity.
func ValidCronAggregate(s string) bool { return s != "identity" && validAggregate(s) }

func validAggregate(s string) bool {
	switch s {
	case "identity", "count", "sum", "avg", "min", "max", "p50", "p95":
		return true
	}
	return false
}

// SplitPipeline detects a `... | chart [flags]` suffix and returns
// the upstream pipeline together with the parsed chart args.
// Returns (line, nil, "") when the line has no chart pipe stage.
// Returns a non-empty errMsg only on flag-parse failure.
//
// Used by non-interactive callers (e.g. weft sh -c "cat foo | chart")
// that don't have a *ShellCore to dispatch through.
func SplitPipeline(line string) (weftPipeline string, args *PipeArgs, errMsg string) {
	idx := strings.LastIndex(line, "|")
	if idx == -1 {
		return line, nil, ""
	}
	tokens := tokenizeArgv(strings.TrimSpace(line[idx+1:]))
	if len(tokens) == 0 || tokens[0] != "chart" {
		return line, nil, ""
	}
	parsed, msg := ParsePipeArgs(tokens[1:])
	if msg != "" {
		return line, nil, msg
	}
	pa := parsed.(PipeArgs)
	return strings.TrimSpace(line[:idx]), &pa, ""
}

// tokenizeArgv splits the chart pipe-stage argv with quote
// unwrapping. Mirrors shellcore.tokenizeArgv (which is unexported)
// but stays scoped to chart's needs (no shell variable expansion
// or escape handling — the upstream pipeline parser has already
// handled those by the time SplitPipeline runs).
func tokenizeArgv(s string) []string {
	var out []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case (c == ' ' || c == '\t') && !inSingle && !inDouble:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return out
}
