package coordinator

import (
	"fmt"
	"io"
	"math"
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

	// Show man page.
	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Fprint(stdout, alertManPage)
		return 0
	}

	// Parse current value as float.
	current, numErr := strconv.ParseFloat(value, 64)
	isNumeric := numErr == nil

	// Get history if needed.
	var history []store.CronResult
	if parsed.duration > 0 && ctx.Store != nil && ctx.Watch != nil {
		history, _ = ctx.Store.ListCronResults(ctx.Watch.ID, 100)
		// Filter to duration window.
		cutoff := time.Now().Add(-parsed.duration)
		var filtered []store.CronResult
		for _, r := range history {
			if r.RanAt.After(cutoff) {
				filtered = append(filtered, r)
			}
		}
		history = filtered
	}

	// Get previous value for changed/rising/falling.
	var prevValue float64
	var hasPrev bool
	if ctx.Store != nil && ctx.Watch != nil {
		results, _ := ctx.Store.ListCronResults(ctx.Watch.ID, 2)
		if len(results) >= 2 {
			if v, err := strconv.ParseFloat(strings.TrimSpace(results[1].Stdout), 64); err == nil {
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
		if parsed.duration > 0 && parsed.average {
			avg := historyAvg(history)
			triggered = avg > parsed.threshold
			reason = fmt.Sprintf("average %.1f > %.1f over %s", avg, parsed.threshold, parsed.duration)
		} else if parsed.duration > 0 {
			triggered = allAbove(history, parsed.threshold)
			reason = fmt.Sprintf("above %.1f for %s", parsed.threshold, parsed.duration)
		} else {
			triggered = current > parsed.threshold
			reason = fmt.Sprintf("%.1f > %.1f", current, parsed.threshold)
		}

	case condBelow:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", value)
			return 1
		}
		if parsed.duration > 0 && parsed.average {
			avg := historyAvg(history)
			triggered = avg < parsed.threshold
			reason = fmt.Sprintf("average %.1f < %.1f over %s", avg, parsed.threshold, parsed.duration)
		} else if parsed.duration > 0 {
			triggered = allBelow(history, parsed.threshold)
			reason = fmt.Sprintf("below %.1f for %s", parsed.threshold, parsed.duration)
		} else {
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
		if hasPrev {
			triggered = value != strings.TrimSpace(history[0].Stdout) // compare raw strings
			if triggered {
				reason = fmt.Sprintf("changed from %q to %q", strings.TrimSpace(history[0].Stdout), value)
			}
		}

	case condAbsent:
		if parsed.duration == 0 {
			fmt.Fprintf(stderr, "alert: absent requires --duration\n")
			return 1
		}
		triggered = len(history) == 0 || value == ""
		reason = fmt.Sprintf("absent for %s", parsed.duration)

	case condRising:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", value)
			return 1
		}
		var baseline float64
		if parsed.duration > 0 && len(history) > 0 {
			baseline = historyAvg(history)
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
		if parsed.duration > 0 && len(history) > 0 {
			baseline = historyAvg(history)
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

// ── History helpers ─────────────────────────────────────────

func historyAvg(results []store.CronResult) float64 {
	if len(results) == 0 {
		return 0
	}
	var sum float64
	var count int
	for _, r := range results {
		if v, err := strconv.ParseFloat(strings.TrimSpace(r.Stdout), 64); err == nil {
			sum += v
			count++
		}
	}
	if count == 0 {
		return 0
	}
	return sum / float64(count)
}

func allAbove(results []store.CronResult, threshold float64) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		v, err := strconv.ParseFloat(strings.TrimSpace(r.Stdout), 64)
		if err != nil || v <= threshold {
			return false
		}
	}
	return true
}

func allBelow(results []store.CronResult, threshold float64) bool {
	if len(results) == 0 {
		return false
	}
	for _, r := range results {
		v, err := strconv.ParseFloat(strings.TrimSpace(r.Stdout), 64)
		if err != nil || v >= threshold {
			return false
		}
	}
	return true
}

// Suppress unused import warning.
var _ = math.Abs

// ── Man page ────────────────────────────────────────────────

const alertManPage = `ALERT(1)                    Weft Commands                    ALERT(1)

NAME
    alert - evaluate conditions and dispatch notifications

SYNOPSIS
    <pipeline> | alert [<condition>] [--notify=<alias>]
                       [--duration <dur>] [--average] [--msg <text>]

DESCRIPTION
    Reads a value from stdin and evaluates it against a condition.
    If the condition is met, writes an alert message to stderr and,
    when --notify is supplied, dispatches the alias from /etc/notify.conf
    to its Slack and/or PagerDuty targets.

    If no condition is given, the default is "any": the alert fires
    whenever stdin is non-empty.

    Always passes stdin through to stdout unchanged (tee behavior).

    In a cron watch (/cron.d/), incidents are deduplicated on
    (watch, condition). Subsequent ticks during an open incident
    update it but don't re-page. When the condition clears, the
    incident closes.

    From the interactive shell, alert is the supported way to
    test an alias end-to-end:
        echo "hello" | alert --notify=ops

CONDITIONS
    any           Fires whenever stdin is non-empty (default)
    above <N>     Current value is greater than N
    below <N>     Current value is less than N
    eq <N>        Current value equals N
    neq <N>       Current value does not equal N
    changed       Value differs from previous run
    absent        No input received (requires --duration)
    rising <N>%   Value increased by N% compared to previous
    falling <N>%  Value decreased by N% compared to previous

OPTIONS
    --notify=<alias>
        Dispatch to the named alias in /etc/notify.conf.
        Without --notify, alert only writes to stderr.

    --duration <dur>
        Time window for sustained conditions. For above/below:
        condition must hold for ALL runs in the window. For
        rising/falling: compares against the window average.
        Required for absent. Examples: 5m, 1h, 30s.

    --average
        For above/below with --duration: compare against the
        rolling average over the window instead of requiring
        every individual run to match.

    --msg <text>
        Override the default alert message.

EXAMPLES
    cat myapp.log --since 5m | awk '$LEVEL == "ERROR"' | alert --notify=ops
    check_errors | alert above 50 --notify=critical
    check_errors | alert above 50 --duration 5m --average --notify=ops
    check_errors | alert rising 100% --notify=critical
    cat /log/canary.log | tail -1 | alert absent --duration 10m --notify=ops
    echo "integration test" | alert --notify=ops

SEE ALSO
    help, vi, cat
`
