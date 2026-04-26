package shellcore

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// AlertCondition enumerates the recognized alert condition keywords.
type AlertCondition int

const (
	AlertAny AlertCondition = iota // fires on any non-empty stdin
	AlertAbove
	AlertBelow
	AlertEq
	AlertNeq
	AlertChanged
	AlertAbsent
	AlertRising
	AlertFalling
)

// String is the lowercase keyword form of a condition as it appears
// in pipelines ("above", "below", …). Used by the dashboard API to
// project a watch's alert condition into JSON — the frontend keys on
// these strings to decide whether to draw a threshold line.
func (c AlertCondition) String() string {
	switch c {
	case AlertAbove:
		return "above"
	case AlertBelow:
		return "below"
	case AlertEq:
		return "eq"
	case AlertNeq:
		return "neq"
	case AlertChanged:
		return "changed"
	case AlertAbsent:
		return "absent"
	case AlertRising:
		return "rising"
	case AlertFalling:
		return "falling"
	default:
		return "any"
	}
}

// AlertArgs is the parsed form of an `alert` command invocation. Kept in
// shellcore so both the runtime (coordinator) and the save-time validator
// (store) can share one parser — the alternative is drifting grammars.
type AlertArgs struct {
	Condition AlertCondition
	Threshold float64       // value for above/below/eq/neq
	PctChange float64       // rising/falling, as a fraction (1.0 = 100%)
	Duration  time.Duration // --duration window; 0 means no window
	Average   bool          // --average flag (above/below with duration)
	Msg       string        // --msg override
	Notify    string        // --notify=<alias> → notify.conf lookup
	Key       FieldSpec     // -k: 1-indexed column or .json.path; zero = "whole line"
	Tag       FieldSpec     // -t/--tag: same shape; zero = no tag
}

// ParseAlertArgs parses the argv following `alert` on the command line.
// Returns an error for malformed flags, unknown conditions, or missing
// required values. Unknown flags are silently ignored to match the prior
// behavior of parseAlertArgs — change carefully.
func ParseAlertArgs(args []string) (*AlertArgs, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: alert [<condition>] --notify=<alias> [-k <N>] [-t <N>] [--duration <dur>] [--average] [--msg <text>]")
	}

	parsed := &AlertArgs{Condition: AlertAny}
	i := 0

	// First positional may be a condition keyword. If it starts with "-" or
	// isn't a known condition, fall through with default (any).
	switch args[0] {
	case "any":
		parsed.Condition = AlertAny
		i++

	case "above":
		parsed.Condition = AlertAbove
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
		parsed.Condition = AlertBelow
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
		parsed.Condition = AlertEq
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
		parsed.Condition = AlertNeq
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
		parsed.Condition = AlertChanged
		i++

	case "absent":
		parsed.Condition = AlertAbsent
		i++

	case "rising":
		parsed.Condition = AlertRising
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
		parsed.Condition = AlertFalling
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
			spec, err := ParseFieldSpec(args[i])
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
			spec, err := ParseFieldSpec(args[i])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", a, err)
			}
			if spec.IsZero() {
				return nil, fmt.Errorf("%s requires a field number or .json.path, got empty value", a)
			}
			parsed.Tag = spec
		}
		i++
	}

	// -k and -t must agree on positional vs JSON-path. Mixing them
	// on the same line is incoherent — one parser sees whitespace
	// columns, the other parses the line as JSON.
	if !parsed.Key.IsZero() && !parsed.Tag.IsZero() && parsed.Key.IsJSON() != parsed.Tag.IsJSON() {
		return nil, fmt.Errorf("-k and -t must both be positional or both .json.path")
	}

	return parsed, nil
}
