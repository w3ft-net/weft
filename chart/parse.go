package chart

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// ParseWatchCmd parses a tokenised `chart …` command line into
// WatchArgs. The first non-flag token is the watch name (bare or
// `/cron.d/<name>`). Returns an error message when the user types
// something we don't grok — the message is what shows up in the
// shell, so it should be terse.
func ParseWatchCmd(args []string) (WatchArgs, string) {
	out := WatchArgs{Limit: 50, Interval: time.Second}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-t" || a == "--type":
			if i+1 >= len(args) {
				return out, "chart: -t requires a value (line, sparkline, bar)"
			}
			out.Kind = args[i+1]
			i++
		case a == "-n" || a == "--limit":
			if i+1 >= len(args) {
				return out, "chart: -n requires a number"
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				return out, "chart: -n must be a positive integer"
			}
			out.Limit = n
			i++
		case a == "-i" || a == "--interval":
			if i+1 >= len(args) {
				return out, "chart: -i requires a duration (e.g. 1s, 500ms)"
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d < 100*time.Millisecond {
				return out, "chart: -i must be a duration ≥ 100ms"
			}
			out.Interval = d
			i++
		case a == "-f" || a == "--follow":
			out.Follow = true
		case strings.HasPrefix(a, "-"):
			return out, fmt.Sprintf("chart: unknown flag %q", a)
		default:
			if out.Name != "" {
				return out, fmt.Sprintf("chart: unexpected argument %q (only one cron name allowed)", a)
			}
			out.Name = a
		}
	}
	if out.Name == "" {
		return out, "usage: chart <cron> [-t TYPE] [-n LIMIT] [-i INTERVAL] [-f]"
	}
	// Strip /cron.d/ prefix so the user can `chart /cron.d/foo` or `chart foo`.
	out.Name = strings.TrimPrefix(out.Name, "/cron.d/")
	out.Name = strings.TrimPrefix(out.Name, "cron.d/")
	if out.Kind == "" {
		out.Kind = "line"
	}
	switch out.Kind {
	case "line", "sparkline", "bar":
	default:
		return out, fmt.Sprintf("chart: unknown type %q (want line, sparkline, bar)", out.Kind)
	}
	return out, ""
}

// PipeArgs holds the parsed flags for `... | chart` (pipe-stage
// form). Distinct from WatchArgs (watch form) — the two parsers
// don't overlap. Exported so non-interactive callers (e.g.
// weft sh -c "...") can carry it across the query/render boundary.
type PipeArgs struct {
	XSpec     shellcore.FieldSpec
	YSpec     shellcore.FieldSpec
	TagSpec   shellcore.FieldSpec // splits output into one line per tag value; line/sparkline only
	Aggregate string        // count | sum | avg | min | max | p50 | p95 | identity
	Bucket    time.Duration // 0 = no bucketing
	Kind      string        // line | bar | sparkline
	Title     string
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
//   - --tag is only supported for -t line (rejected with bar/sparkline).
//
// Signature matches shellcore.PipeStageParseFunc — this function
// is registered directly via core.RegisterPipeStage("chart", ...).
func ParsePipeArgs(args []string) (any, string) {
	out := PipeArgs{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-x":
			if i+1 >= len(args) {
				return out, "chart: -x requires a column index or .json.path"
			}
			s, err := shellcore.ParseFieldSpec(args[i+1])
			if err != nil {
				return out, "chart: -x: " + err.Error()
			}
			out.XSpec = s
			i++
		case a == "-y":
			if i+1 >= len(args) {
				return out, "chart: -y requires a column index or .json.path"
			}
			s, err := shellcore.ParseFieldSpec(args[i+1])
			if err != nil {
				return out, "chart: -y: " + err.Error()
			}
			out.YSpec = s
			i++
		case a == "--tag":
			if i+1 >= len(args) {
				return out, "chart: --tag requires a column index or .json.path"
			}
			s, err := shellcore.ParseFieldSpec(args[i+1])
			if err != nil {
				return out, "chart: --tag: " + err.Error()
			}
			out.TagSpec = s
			i++
		case a == "-a" || a == "--aggregate":
			if i+1 >= len(args) {
				return out, "chart: -a requires a function (count|sum|avg|min|max|p50|p95)"
			}
			out.Aggregate = args[i+1]
			i++
		case a == "--bucket":
			if i+1 >= len(args) {
				return out, "chart: --bucket requires a duration (e.g. 1m, 30s)"
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil || d <= 0 {
				return out, fmt.Sprintf("chart: --bucket: invalid duration %q", args[i+1])
			}
			out.Bucket = d
			i++
		case a == "-t" || a == "--type":
			if i+1 >= len(args) {
				return out, "chart: -t requires line|bar|sparkline"
			}
			out.Kind = args[i+1]
			i++
		case a == "--title":
			if i+1 >= len(args) {
				return out, "chart: --title requires a value"
			}
			out.Title = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			return out, fmt.Sprintf("chart: unknown flag %q", a)
		default:
			return out, fmt.Sprintf("chart: unexpected argument %q (use flags only in pipe mode)", a)
		}
	}

	// Mixing positional and JSON-path is rejected — the line parser
	// can't be in two modes at once.
	specs := []shellcore.FieldSpec{}
	for _, s := range []shellcore.FieldSpec{out.XSpec, out.YSpec, out.TagSpec} {
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
		return out, fmt.Sprintf("chart: unknown type %q (want line|bar|sparkline)", out.Kind)
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
