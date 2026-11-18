// Package alert parses the `alert` builtin's argument vector.
//
// Used both by the interactive shell (where `<pipeline> | alert
// …` evaluates the upstream output) and by the cron executor
// (which evaluates persisted watch results on a schedule).
//
// The parser is decoupled from the executor: ParseArgs returns a
// pure value object; the actual evaluation lives in
// weft/coordinator/alert.go.
package alert

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// errSecondNotify rejects a second --notify. An alert pages one alias; the
// last of several used to win silently, so a cron meant to page two places
// paged one.
var errSecondNotify = errors.New("--notify may be given only once per alert: point it at one notify.yaml alias with several targets, or use one cron per alias")

// Condition enumerates the recognized alert condition keywords.
type Condition int

const (
	Any Condition = iota // fires on any non-empty stdin
	Above
	Below
	Eq
	Neq
	Changed
	Absent
	Rising
	Falling
	Matches
)

// String is the lowercase keyword form of a condition as it appears
// in pipelines ("above", "below", …). Used by the dashboard API to
// project a watch's alert condition into JSON — the frontend keys on
// these strings to decide whether to draw a threshold line.
func (c Condition) String() string {
	switch c {
	case Above:
		return "above"
	case Below:
		return "below"
	case Eq:
		return "eq"
	case Neq:
		return "neq"
	case Changed:
		return "changed"
	case Absent:
		return "absent"
	case Rising:
		return "rising"
	case Falling:
		return "falling"
	case Matches:
		return "matches"
	default:
		return "any"
	}
}

// Args is the parsed form of an `alert` command invocation. Kept in
// shellcore so both the runtime (coordinator) and the save-time validator
// (store) can share one parser — the alternative is drifting grammars.
type Args struct {
	Condition Condition
	Threshold float64             // value for above/below/eq/neq
	PctChange float64             // rising/falling, as a fraction (1.0 = 100%)
	Pattern   *regexp.Regexp      // matches
	Duration  time.Duration       // --duration window; 0 means no window
	Average   bool                // --average flag (above/below with duration)
	Msg       string              // --msg override
	Notify    string              // --notify=<alias> → notify.yaml lookup
	Key       shellcore.FieldSpec // -v: 1-indexed column or .json.path; zero = "whole line"
	Tag       shellcore.FieldSpec // -t/--tag: same shape; zero = no tag
	// IfMode/IfValue support `alert --if-zero "$?"` and
	// `alert --if-nonzero "$?"` — a cron composes predicates with
	// bash &&/||, then passes the resulting exit code to alert
	// directly, no `echo $? | alert below 1` plumbing.
	IfMode  IfMode // IfNone | IfZero | IfNonzero
	IfValue string // the captured value (typically "$?" → "0" or "1")
}

// IfMode selects between exit-code modes for `alert --if-zero` /
// `alert --if-nonzero`. IfNone means use the stdin-condition path
// (the default — every condition keyword in this file).
type IfMode int

const (
	IfNone IfMode = iota
	IfZero
	IfNonzero
)

// Spec declares the `alert` command line: an optional condition keyword
// (with its value) and the flags. Options may come before or after the
// keyword.
var Spec = bash.Spec{
	Name:    "alert",
	Summary: "pass input through and fire when a condition holds",
	Description: "CONDITION is one of: any, above N, below N, eq N, neq N, changed, absent,\n" +
		"rising PCT%, falling PCT%, matches REGEX. With none it fires on any non-empty input.\n" +
		"--if-zero and --if-nonzero take an exit status (\"$?\") and are exclusive with a condition.",
	Flags: []bash.Flag{
		bash.String("duration", 0, "evaluate over a window of DUR (e.g. 5m)").Arg("DUR"),
		bash.Bool("average", 0, "with above/below and --duration, compare the window average"),
		bash.String("msg", 0, "replace the alert message").Arg("TEXT"),
		bash.StringList("notify", 0, "page this notify.yaml alias (once per alert)").Arg("ALIAS"),
		bash.String("value", 'v', "evaluate each line's column or .json.path").Arg("FIELD"),
		bash.String("tag", 't', "name each row by this column or .json.path").Arg("FIELD"),
		bash.String("if-zero", 0, "fire when VALUE (typically \"$?\") is 0").Arg("VALUE"),
		bash.String("if-nonzero", 0, "fire when VALUE (typically \"$?\") is not 0").Arg("VALUE"),
	},
	Args:            bash.Operands("CONDITION", "VALUE").Any().Complete(bash.NoCompletion),
	Legacy:          valueAfterKeyword,
	Strategy:        bash.StageBarrier,
	Aggregating:     true,
	NumericOperands: true, // `below -5`
}

// conditionValues is how many values each condition keyword takes.
var conditionValues = map[string]int{
	"any": 0, "above": 1, "below": 1, "eq": 1, "neq": 1,
	"changed": 0, "absent": 0, "rising": 1, "falling": 1, "matches": 1,
}

// valueAfterKeyword protects the value that follows a condition keyword from
// being read as an option, as it never was: `alert matches '-rc[0-9]*$'` is a
// pattern, not a flag cluster. The value moves behind a `--`.
func valueAfterKeyword(args []string) []string {
	if len(args) < 2 || conditionValues[args[0]] == 0 {
		return args
	}
	out := append([]string{args[0]}, args[2:]...)
	return append(out, "--", args[1])
}

// ParseArgs parses the argv following `alert` on the command line.
// Returns an error for malformed flags, unknown conditions, or missing
// required values, and for options it does not know.
func ParseArgs(args []string) (*Args, error) {
	p, err := Spec.Parse(args)
	if err != nil {
		// The caller prefixes "alert: ", so drop the spec's own.
		var ue *bash.UsageError
		if errors.As(err, &ue) {
			return nil, errors.New(ue.Msg)
		}
		return nil, err
	}
	if p.Help {
		return nil, errors.New(strings.TrimRight(Spec.Help(), "\n"))
	}
	return ArgsFrom(p)
}

// ArgsFrom reads an `alert` line already parsed against Spec.
func ArgsFrom(p *bash.Parsed) (*Args, error) {
	parsed := &Args{Condition: Any}
	ops := p.Operands()
	if len(ops) > 0 {
		kw, vals := ops[0], ops[1:]
		want, ok := conditionValues[kw]
		if !ok {
			return nil, fmt.Errorf("unknown condition: %s", kw)
		}
		if len(vals) < want {
			switch kw {
			case "above", "below":
				return nil, fmt.Errorf("%s requires a threshold", kw)
			case "eq", "neq":
				return nil, fmt.Errorf("%s requires a value", kw)
			case "rising":
				return nil, fmt.Errorf("rising requires a percentage (e.g., 100%%)")
			case "falling":
				return nil, fmt.Errorf("falling requires a percentage (e.g., 50%%)")
			default:
				return nil, fmt.Errorf("matches requires a regular expression")
			}
		}
		if len(vals) > want {
			return nil, fmt.Errorf("unexpected argument %q", vals[want])
		}
		switch kw {
		case "any":
			parsed.Condition = Any
		case "above", "below", "eq", "neq":
			parsed.Condition = map[string]Condition{"above": Above, "below": Below, "eq": Eq, "neq": Neq}[kw]
			v, err := strconv.ParseFloat(vals[0], 64)
			if err != nil {
				what := "threshold"
				if kw == "eq" || kw == "neq" {
					what = "value"
				}
				return nil, fmt.Errorf("invalid %s: %s", what, vals[0])
			}
			parsed.Threshold = v
		case "changed":
			parsed.Condition = Changed
		case "absent":
			parsed.Condition = Absent
		case "rising", "falling":
			parsed.Condition = map[string]Condition{"rising": Rising, "falling": Falling}[kw]
			v, err := strconv.ParseFloat(strings.TrimSuffix(vals[0], "%"), 64)
			if err != nil {
				return nil, fmt.Errorf("invalid percentage: %s", vals[0])
			}
			parsed.PctChange = v / 100.0
		case "matches":
			parsed.Condition = Matches
			re, err := regexp.Compile(vals[0])
			if err != nil {
				return nil, fmt.Errorf("invalid pattern %q: %w", vals[0], err)
			}
			parsed.Pattern = re
		}
	}

	if p.Has("duration") {
		d, err := time.ParseDuration(p.String("duration"))
		if err != nil {
			return nil, fmt.Errorf("invalid duration: %s", p.String("duration"))
		}
		parsed.Duration = d
	}
	parsed.Average = p.Bool("average")
	parsed.Msg = p.String("msg")
	if n := p.List("notify"); len(n) > 1 {
		return nil, errSecondNotify
	} else if len(n) == 1 {
		parsed.Notify = n[0]
	}
	for _, f := range []struct {
		name, label string
		dst         *shellcore.FieldSpec
	}{{"value", "-v", &parsed.Key}, {"tag", "-t", &parsed.Tag}} {
		if !p.Has(f.name) {
			continue
		}
		spec, err := shellcore.ParseFieldSpec(p.String(f.name))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f.label, err)
		}
		if spec.IsZero() {
			return nil, fmt.Errorf("%s requires a field number or .json.path, got empty value", f.label)
		}
		*f.dst = spec
	}
	switch {
	case p.Has("if-zero"):
		parsed.IfMode, parsed.IfValue = IfZero, p.String("if-zero")
	case p.Has("if-nonzero"):
		parsed.IfMode, parsed.IfValue = IfNonzero, p.String("if-nonzero")
	}

	if parsed.IfMode != IfNone && parsed.Condition != Any {
		return nil, fmt.Errorf("--if-zero/--if-nonzero is exclusive with condition keywords")
	}

	// -v and -t must agree on positional vs JSON-path. Mixing them
	// on the same line is incoherent — one parser sees whitespace
	// columns, the other parses the line as JSON.
	if !parsed.Key.IsZero() && !parsed.Tag.IsZero() && parsed.Key.IsJSON() != parsed.Tag.IsJSON() {
		return nil, fmt.Errorf("-v and -t must both be positional or both .json.path")
	}

	return parsed, nil
}
