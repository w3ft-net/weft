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
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

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
	Duration  time.Duration       // --duration window; 0 means no window
	Average   bool                // --average flag (above/below with duration)
	Msg       string              // --msg override
	Notify    string              // --notify=<alias> → notify.yaml lookup
	Key       shellcore.FieldSpec // -k: 1-indexed column or .json.path; zero = "whole line"
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

// ParseArgs parses the argv following `alert` on the command line.
// Returns an error for malformed flags, unknown conditions, or missing
// required values. Unknown flags are silently ignored to match the prior
// behavior of parseArgs — change carefully.
func ParseArgs(args []string) (*Args, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: alert [<condition>] --notify=<alias> [-k <N>] [-t <N>] [--duration <dur>] [--average] [--msg <text>]")
	}

	parsed := &Args{Condition: Any}
	i := 0

	// First positional may be a condition keyword. If it starts with "-" or
	// isn't a known condition, fall through with default (any).
	switch args[0] {
	case "any":
		parsed.Condition = Any
		i++

	case "above":
		parsed.Condition = Above
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("above requires a threshold")
		}
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid threshold: %s", args[i])
		}
		parsed.Threshold = v
		i++

	case "below":
		parsed.Condition = Below
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("below requires a threshold")
		}
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid threshold: %s", args[i])
		}
		parsed.Threshold = v
		i++

	case "eq":
		parsed.Condition = Eq
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("eq requires a value")
		}
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid value: %s", args[i])
		}
		parsed.Threshold = v
		i++

	case "neq":
		parsed.Condition = Neq
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("neq requires a value")
		}
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid value: %s", args[i])
		}
		parsed.Threshold = v
		i++

	case "changed":
		parsed.Condition = Changed
		i++

	case "absent":
		parsed.Condition = Absent
		i++

	case "rising":
		parsed.Condition = Rising
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("rising requires a percentage (e.g., 100%%)")
		}
		pct := strings.TrimSuffix(args[i], "%")
		v, err := strconv.ParseFloat(pct, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid percentage: %s", args[i])
		}
		parsed.PctChange = v / 100.0
		i++

	case "falling":
		parsed.Condition = Falling
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("falling requires a percentage (e.g., 50%%)")
		}
		pct := strings.TrimSuffix(args[i], "%")
		v, err := strconv.ParseFloat(pct, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid percentage: %s", args[i])
		}
		parsed.PctChange = v / 100.0
		i++

	default:
		if !strings.HasPrefix(args[0], "-") {
			return nil, fmt.Errorf("unknown condition: %s", args[0])
		}
		// First token is a flag — fall through with default condition (any).
	}

	// Parse flags.
	for i < len(args) {
		a := args[i]
		// Support --notify=foo as well as --notify foo.
		if strings.HasPrefix(a, "--notify=") {
			parsed.Notify = strings.TrimPrefix(a, "--notify=")
			i++
			continue
		}
		switch a {
		case "--duration":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--duration requires a value")
			}
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return nil, fmt.Errorf("invalid duration: %s", args[i])
			}
			parsed.Duration = d
		case "--average":
			parsed.Average = true
		case "--msg":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--msg requires a value")
			}
			parsed.Msg = args[i]
		case "--notify":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--notify requires a value")
			}
			parsed.Notify = args[i]
		case "-k":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("-k requires a field number or .json.path")
			}
			spec, err := shellcore.ParseFieldSpec(args[i])
			if err != nil {
				return nil, fmt.Errorf("-k: %w", err)
			}
			if spec.IsZero() {
				return nil, fmt.Errorf("-k requires a field number or .json.path, got empty value")
			}
			parsed.Key = spec
		case "-t", "--tag":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("%s requires a field number or .json.path", a)
			}
			spec, err := shellcore.ParseFieldSpec(args[i])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", a, err)
			}
			if spec.IsZero() {
				return nil, fmt.Errorf("%s requires a field number or .json.path, got empty value", a)
			}
			parsed.Tag = spec
		case "--if-zero":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--if-zero requires a value (typically \"$?\")")
			}
			parsed.IfMode = IfZero
			parsed.IfValue = args[i]
		case "--if-nonzero":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--if-nonzero requires a value (typically \"$?\")")
			}
			parsed.IfMode = IfNonzero
			parsed.IfValue = args[i]
		}
		i++
	}

	if parsed.IfMode != IfNone && parsed.Condition != Any {
		return nil, fmt.Errorf("--if-zero/--if-nonzero is exclusive with condition keywords")
	}

	// -k and -t must agree on positional vs JSON-path. Mixing them
	// on the same line is incoherent — one parser sees whitespace
	// columns, the other parses the line as JSON.
	if !parsed.Key.IsZero() && !parsed.Tag.IsZero() && parsed.Key.IsJSON() != parsed.Tag.IsJSON() {
		return nil, fmt.Errorf("-k and -t must both be positional or both .json.path")
	}

	return parsed, nil
}
