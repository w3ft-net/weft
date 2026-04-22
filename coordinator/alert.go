package coordinator

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/kevlarcon/weft/store"
)

// AlertContext provides the alert command with access to result history,
// incident dedup, and the Notifier. Watch is nil for interactive runs,
// which skips the (watchID, condition) dedup check but still dispatches
// if --notify is supplied — useful for testing aliases from the shell.
type AlertContext struct {
	Store       *store.Store
	Watch       *store.CronWatch
	Notifier    *Notifier
	CustomerID  uint
	Interactive bool
}

type alertCondition int

const (
	condAny alertCondition = iota // fires on any non-empty stdin
	condAbove
	condBelow
	condEq
	condNeq
	condChanged
	condAbsent
	condRising
	condFalling
)

type alertArgs struct {
	condition alertCondition
	threshold float64
	pctChange float64 // for rising/falling, as a fraction (1.0 = 100%)
	duration  time.Duration
	average   bool
	msg       string
	notify    string // alias name in /etc/notify.conf
}

// AlertCommand is the entry point for the `alert` pipeline command.
// It reads all of stdin, writes it to stdout (tee), evaluates the condition,
// and writes to stderr if triggered.
func AlertCommand(args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, ctx *AlertContext) int {
	// Read all stdin.
	data, _ := io.ReadAll(stdin)
	value := strings.TrimSpace(string(data))

	// Always pass through to stdout (tee behavior).
	stdout.Write(data)

	// Parse args.
	parsed, err := parseAlertArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "alert: %v\n", err)
		return 1
	}

	// Parse current value as float.
	current, numErr := strconv.ParseFloat(value, 64)
	isNumeric := numErr == nil

	// Fetch window aggregates (avg/min/max/samples) once, if needed.
	var stats store.CronAggStats
	if parsed.duration > 0 && ctx.Store != nil && ctx.Watch != nil {
		stats, _ = ctx.Store.CronAggregate(ctx.Watch.ID, time.Now().Add(-parsed.duration))
	}

	// Previous value for changed/rising/falling (single prior row).
	var prevValue float64
	var prevStdout string
	var hasPrev bool
	if ctx.Store != nil && ctx.Watch != nil {
		results, _ := ctx.Store.ListCronResults(ctx.Watch.ID, 2)
		if len(results) >= 2 {
			prevStdout = strings.TrimSpace(results[1].Stdout)
			if v, err := strconv.ParseFloat(prevStdout, 64); err == nil {
				prevValue = v
				hasPrev = true
			}
		}
	}

	// Evaluate.
	triggered := false
	var reason string

	switch parsed.condition {
	case condAny:
		triggered = value != ""
		if triggered {
			reason = "non-empty output"
		}

	case condAbove:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", value)
			return 1
		}
		if parsed.duration > 0 && parsed.average && stats.Avg != nil {
			triggered = *stats.Avg > parsed.threshold
			reason = fmt.Sprintf("average %.1f > %.1f over %s", *stats.Avg, parsed.threshold, parsed.duration)
		} else if parsed.duration > 0 && stats.Min != nil {
			// MIN > threshold ⇒ all samples > threshold.
			triggered = *stats.Min > parsed.threshold
			reason = fmt.Sprintf("above %.1f for %s", parsed.threshold, parsed.duration)
		} else if parsed.duration == 0 {
			triggered = current > parsed.threshold
			reason = fmt.Sprintf("%.1f > %.1f", current, parsed.threshold)
		}

	case condBelow:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", value)
			return 1
		}
		if parsed.duration > 0 && parsed.average && stats.Avg != nil {
			triggered = *stats.Avg < parsed.threshold
			reason = fmt.Sprintf("average %.1f < %.1f over %s", *stats.Avg, parsed.threshold, parsed.duration)
		} else if parsed.duration > 0 && stats.Max != nil {
			// MAX < threshold ⇒ all samples < threshold.
			triggered = *stats.Max < parsed.threshold
			reason = fmt.Sprintf("below %.1f for %s", parsed.threshold, parsed.duration)
		} else if parsed.duration == 0 {
			triggered = current < parsed.threshold
			reason = fmt.Sprintf("%.1f < %.1f", current, parsed.threshold)
		}

	case condEq:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", value)
			return 1
		}
		triggered = current == parsed.threshold
		reason = fmt.Sprintf("%.1f == %.1f", current, parsed.threshold)

	case condNeq:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", value)
			return 1
		}
		triggered = current != parsed.threshold
		reason = fmt.Sprintf("%.1f != %.1f", current, parsed.threshold)

	case condChanged:
		if hasPrev || prevStdout != "" {
			triggered = value != prevStdout
			if triggered {
				reason = fmt.Sprintf("changed from %q to %q", prevStdout, value)
			}
		}

	case condAbsent:
		if parsed.duration == 0 {
			triggered = value == ""
		} else if ctx.Store != nil && ctx.Watch != nil {
			n, _ := ctx.Store.CronNonEmptyCount(ctx.Watch.ID, time.Now().Add(-parsed.duration))
			triggered = n == 0
		} else {
			triggered = value == ""
		}
		reason = fmt.Sprintf("absent for %s", parsed.duration)

	case condRising:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", value)
			return 1
		}
		var baseline float64
		if parsed.duration > 0 && stats.Avg != nil {
			baseline = *stats.Avg
		} else if hasPrev {
			baseline = prevValue
		} else {
			break // no baseline, can't evaluate
		}
		if baseline > 0 {
			pctChange := (current - baseline) / baseline
			triggered = pctChange >= parsed.pctChange
			reason = fmt.Sprintf("rose %.0f%% (from %.1f to %.1f)", pctChange*100, baseline, current)
		}

	case condFalling:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", value)
			return 1
		}
		var baseline float64
		if parsed.duration > 0 && stats.Avg != nil {
			baseline = *stats.Avg
		} else if hasPrev {
			baseline = prevValue
		} else {
			break
		}
		if baseline > 0 {
			pctChange := (baseline - current) / baseline
			triggered = pctChange >= parsed.pctChange
			reason = fmt.Sprintf("fell %.0f%% (from %.1f to %.1f)", pctChange*100, baseline, current)
		}
	}

	if !triggered {
		return 0
	}

	// Build alert message with appropriate prefix.
	msg := parsed.msg
	if msg == "" {
		prefix := "weft"
		if ctx.Watch != nil {
			prefix = ctx.Watch.Name
		}
		msg = prefix + ": " + reason
	}

	// Incident dedup (cron mode only). If the incident is already open we
	// write to stderr for visibility but skip dispatch to avoid re-paging.
	shouldDispatch := true
	if ctx.Watch != nil && ctx.Store != nil {
		condStr := strings.Join(args, " ")
		shouldDispatch = checkIncident(ctx.Store, ctx.Watch.ID, condStr, true)
	}

	// Always write to stderr so the cron executor logs the trigger and the
	// shell user sees it interactively.
	fmt.Fprintln(stderr, msg)

	// Dispatch through the Notifier if an alias was supplied.
	if shouldDispatch && parsed.notify != "" && ctx.Notifier != nil {
		body := truncateAlertBody(value)
		watchName, customerID := ctx.dispatchIdentity()
		if err := ctx.Notifier.Send(customerID, parsed.notify, watchName, body); err != nil {
			fmt.Fprintf(stderr, "alert: dispatch to %q failed: %v\n", parsed.notify, err)
			return 1
		}
	} else if parsed.notify != "" && ctx.Notifier == nil && ctx.Interactive {
		fmt.Fprintf(stderr, "alert: notifier unavailable — would dispatch to %q\n", parsed.notify)
	}
	return 0
}

// dispatchIdentity returns the (watchName, customerID) used by the Notifier
// for dedup-key construction. For ad-hoc shell runs a unique synthetic name
// ensures every invocation creates its own PagerDuty incident instead of
// collapsing into a shared "adhoc" key.
func (ctx *AlertContext) dispatchIdentity() (string, uint) {
	if ctx.Watch != nil {
		return ctx.Watch.Name, ctx.Watch.CustomerID
	}
	return fmt.Sprintf("adhoc-%d", time.Now().UnixNano()), ctx.CustomerID
}

// truncateAlertBody caps stdin content at 1 KB for the alert body. Slack and
// PagerDuty have separate hard caps inside the Notifier; this one just keeps
// us honest about what gets shipped over the wire.
func truncateAlertBody(s string) string {
	const max = 1024
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}

// checkIncident handles incident dedup. Returns true if this is a NEW incident
// (should notify). Returns false if an incident is already open (suppress).
func checkIncident(s *store.Store, watchID uint, condition string, triggered bool) bool {
	existing, _ := s.OpenIncident(watchID, condition)

	if triggered {
		if existing != nil {
			// Already open — just update seen time.
			s.UpdateIncidentSeen(existing.ID)
			return false
		}
		// New incident — create and signal notification.
		s.CreateIncident(watchID, condition)
		return true
	}

	// Not triggered — close any open incident.
	if existing != nil {
		s.CloseIncident(existing.ID)
	}
	return false
}

// ── Arg parsing ─────────────────────────────────────────────

func parseAlertArgs(args []string) (*alertArgs, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("usage: alert [<condition>] --notify=<alias> [--duration <dur>] [--average] [--msg <text>]")
	}

	parsed := &alertArgs{condition: condAny}
	i := 0

	// First positional may be a condition keyword. If it starts with "--" or
	// isn't a known condition, fall through with default (any).
	switch args[0] {
	case "any":
		parsed.condition = condAny
		i++

	case "above":
		parsed.condition = condAbove
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("above requires a threshold")
		}
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid threshold: %s", args[i])
		}
		parsed.threshold = v
		i++

	case "below":
		parsed.condition = condBelow
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("below requires a threshold")
		}
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid threshold: %s", args[i])
		}
		parsed.threshold = v
		i++

	case "eq":
		parsed.condition = condEq
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("eq requires a value")
		}
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid value: %s", args[i])
		}
		parsed.threshold = v
		i++

	case "neq":
		parsed.condition = condNeq
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("neq requires a value")
		}
		v, err := strconv.ParseFloat(args[i], 64)
		if err != nil {
			return nil, fmt.Errorf("invalid value: %s", args[i])
		}
		parsed.threshold = v
		i++

	case "changed":
		parsed.condition = condChanged
		i++

	case "absent":
		parsed.condition = condAbsent
		i++

	case "rising":
		parsed.condition = condRising
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("rising requires a percentage (e.g., 100%%)")
		}
		pct := strings.TrimSuffix(args[i], "%")
		v, err := strconv.ParseFloat(pct, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid percentage: %s", args[i])
		}
		parsed.pctChange = v / 100.0 // convert to fraction
		i++

	case "falling":
		parsed.condition = condFalling
		i++
		if i >= len(args) {
			return nil, fmt.Errorf("falling requires a percentage (e.g., 50%%)")
		}
		pct := strings.TrimSuffix(args[i], "%")
		v, err := strconv.ParseFloat(pct, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid percentage: %s", args[i])
		}
		parsed.pctChange = v / 100.0
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
			parsed.notify = strings.TrimPrefix(a, "--notify=")
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
			parsed.duration = d
		case "--average":
			parsed.average = true
		case "--msg":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--msg requires a value")
			}
			parsed.msg = args[i]
		case "--notify":
			i++
			if i >= len(args) {
				return nil, fmt.Errorf("--notify requires a value")
			}
			parsed.notify = args[i]
		}
		i++
	}

	return parsed, nil
}

