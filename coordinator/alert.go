package coordinator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"github.com/w3ft-net/weft/fieldspec"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/w3ft-net/weft/identity"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/alert"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/shape"
)

// NotifyDispatcher is the subset of Notifier the alert stage calls.
// Narrowed to an interface so tests can intercept Send without a live
// HTTP endpoint. The concrete *Notifier satisfies it.
type NotifyDispatcher interface {
	Send(ctx context.Context, owner shape.Added, aliasName, watchName, body string, delivered map[string]bool) (sent []string, err error)
}

// AlertContext provides the alert command with access to result history,
// incident dedup, and the Notifier. Watch is nil for interactive runs,
// which skips (watchID, condition, tag) dedup but still dispatches if
// --notify is supplied — useful for testing aliases from the shell.
type AlertContext struct {
	// Crons is where the watch's results and incidents live (they are keyed by
	// watch, so it needs no owner). Nil: there is no history to read or record,
	// as in a coordinator built without a store.
	Crons cron.Store
	// Sources names the watch's namespace in the alert's identity.
	Sources  source.Store
	Watch    *cron.Watch
	Notifier NotifyDispatcher
	// Owner is whose alert it is.
	Owner shape.Added
	// Limits are the group's, for the history the alert reads.
	Limits      identity.Limits
	Interactive bool
}

// tracked is whether the alert is a cron's and has history to read and record.
func (ac *AlertContext) tracked() bool { return ac.Watch != nil && ac.Crons != nil }

// AlertCommand is the entry point for the `alert` pipeline command. It reads
// stdin, writes it to stdout unchanged (tee), and evaluates the alert
// condition. In single-row mode (no -v), stdin is one scalar per tick. In
// multi-row mode (-v set), each non-blank line of stdin is evaluated
// independently, with the tag (if -t set) naming the row for per-tag
// incident dedup.
func AlertCommand(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer, ac *AlertContext) int {
	data, _ := io.ReadAll(stdin)
	stdout.Write(data)

	parsed, err := alert.ParseArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "alert: %v\n", err)
		return 1
	}

	// --if-zero / --if-nonzero short-circuit the stdin path. The cron
	// composed predicates with bash &&/|| and the resulting exit code
	// arrived via `"$?"`. Treat that as the trigger signal directly.
	if parsed.IfMode != alert.IfNone {
		v := strings.TrimSpace(parsed.IfValue)
		fired := false
		switch parsed.IfMode {
		case alert.IfZero:
			fired = v == "0"
		case alert.IfNonzero:
			fired = v != "0" && v != ""
		}
		return evaluateIfMode(ctx, fired, parsed, ac, stderr)
	}

	rows := splitAlertRows(string(data), parsed)

	if !parsed.Tag.IsZero() && len(rows) > 1 {
		for _, r := range rows {
			if r.tag == "" {
				fmt.Fprintf(stderr, "alert: skipping row with empty tag: %q\n", r.raw)
			}
		}
	}

	// In multi-row mode (Key set), each row is a separate evaluation. In
	// single-row mode we collapse to exactly one row whose value is the whole
	// trimmed stdin — keeps the legacy scalar path unchanged.
	exit := 0
	for _, r := range rows {
		if !parsed.Key.IsZero() && !parsed.Tag.IsZero() && r.tag == "" {
			continue
		}
		rc := evaluateAlertRow(ctx, r, parsed, ac, stderr)
		if rc > exit {
			exit = rc
		}
	}
	return exit
}

// alertRow is one evaluation unit — either a whole-stdin scalar (single-row
// mode) or one whitespace-split line (multi-row). The raw string is kept
// for message bodies and error reporting; value/tag are the extracted
// fields.
type alertRow struct {
	raw   string
	value string
	tag   string
}

// splitAlertRows turns stdin into rows per the parser's extractors. Blank
// lines are skipped; lines that fail field extraction produce a row with
// an empty value/tag (evaluated as non-numeric downstream). When -v uses
// a JSON-path spec each line is parsed as JSON; for positional specs
// each line is split on whitespace.
func splitAlertRows(data string, parsed *alert.Args) []alertRow {
	if parsed.Key.IsZero() {
		return []alertRow{{raw: strings.TrimSpace(data), value: strings.TrimSpace(data)}}
	}
	var rows []alertRow
	scanner := bufio.NewScanner(strings.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		if trim == "" {
			continue
		}
		r := alertRow{raw: trim}
		if v, err := fieldspec.ResolveField(parsed.Key, trim); err == nil {
			r.value = v
		}
		if !parsed.Tag.IsZero() {
			if t, err := fieldspec.ResolveField(parsed.Tag, trim); err == nil {
				r.tag = t
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// windowAgg is the aggregate of a cron's numeric rows over an alert's window.
// Avg, Min and Max are nil when Samples is 0.
type windowAgg struct {
	Avg     *float64
	Min     *float64
	Max     *float64
	Samples int
}

// evaluateAlertRow runs condition evaluation + dedup + dispatch for one row.
// Returns 1 on a hard parse/eval error that the caller should propagate as
// the exit code.
func evaluateAlertRow(ctx context.Context, r alertRow, parsed *alert.Args, ac *AlertContext, stderr io.Writer) int {
	current, numErr := strconv.ParseFloat(r.value, 64)
	isNumeric := numErr == nil

	// Window aggregates (per-tag when we have one).
	var stats windowAgg
	if parsed.Duration > 0 && ac.tracked() {
		stats, _ = func(cronWatchID uint, since time.Time, tag string) (windowAgg, error) {
			since = ac.Limits.Clamp(since)
			w, err := ac.Crons.WindowStats(cronWatchID, since, tag)
			if err != nil || w.Count == 0 {
				return windowAgg{}, err
			}
			avg, min, max := w.Mean(), w.Min, w.Max
			return windowAgg{Avg: &avg, Min: &min, Max: &max, Samples: int(w.Count)}, nil
		}(ac.Watch.ID, time.Now().Add(-parsed.Duration), r.tag)
	}

	// Previous tick's value for this same tag (changed/rising/falling).
	// The current tick isn't stored until persistResults runs after the
	// pipeline, so the newest stored row is the previous tick.
	var prevValue float64
	var prevRaw string
	var hasPrev bool
	if ac.tracked() {
		results, err := ac.Crons.ListResults(ac.Watch.ID, 1, r.tag)
		if err != nil {
			weftsdk.Infof(ctx, "alert: ListCronResults watch=%d tag=%q: %v", ac.Watch.ID, r.tag, err)
		}
		if len(results) == 1 {
			prevRaw = strings.TrimSpace(results[0].Stdout)
			switch {
			case results[0].Value != nil:
				prevRaw = strconv.FormatFloat(*results[0].Value, 'f', -1, 64)
			case results[0].TextValue != nil:
				prevRaw = *results[0].TextValue
			}
			if v, err := strconv.ParseFloat(prevRaw, 64); err == nil {
				prevValue = v
				hasPrev = true
			}
		}
	}

	triggered := false
	var reason string

	switch parsed.Condition {
	case alert.Any:
		triggered = r.value != ""
		if triggered {
			reason = "non-empty output"
		}

	case alert.Above:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", r.value)
			return 1
		}
		switch {
		case parsed.Duration > 0 && parsed.Average && stats.Avg != nil:
			triggered = *stats.Avg > parsed.Threshold
			reason = fmt.Sprintf("average %.1f > %.1f over %s", *stats.Avg, parsed.Threshold, parsed.Duration)
		case parsed.Duration > 0 && stats.Min != nil:
			triggered = *stats.Min > parsed.Threshold
			reason = fmt.Sprintf("above %.1f for %s", parsed.Threshold, parsed.Duration)
		case parsed.Duration == 0:
			triggered = current > parsed.Threshold
			reason = fmt.Sprintf("%.1f > %.1f", current, parsed.Threshold)
		}

	case alert.Below:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", r.value)
			return 1
		}
		switch {
		case parsed.Duration > 0 && parsed.Average && stats.Avg != nil:
			triggered = *stats.Avg < parsed.Threshold
			reason = fmt.Sprintf("average %.1f < %.1f over %s", *stats.Avg, parsed.Threshold, parsed.Duration)
		case parsed.Duration > 0 && stats.Max != nil:
			triggered = *stats.Max < parsed.Threshold
			reason = fmt.Sprintf("below %.1f for %s", parsed.Threshold, parsed.Duration)
		case parsed.Duration == 0:
			triggered = current < parsed.Threshold
			reason = fmt.Sprintf("%.1f < %.1f", current, parsed.Threshold)
		}

	case alert.Eq:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", r.value)
			return 1
		}
		triggered = current == parsed.Threshold
		reason = fmt.Sprintf("%.1f == %.1f", current, parsed.Threshold)

	case alert.Neq:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", r.value)
			return 1
		}
		triggered = current != parsed.Threshold
		reason = fmt.Sprintf("%.1f != %.1f", current, parsed.Threshold)

	case alert.Changed:
		if prevRaw != "" {
			if isNumeric && hasPrev {
				triggered = current != prevValue
			} else {
				triggered = r.value != prevRaw
			}
			if triggered {
				reason = fmt.Sprintf("changed from %q to %q", prevRaw, r.value)
			}
		}

	case alert.Matches:
		triggered = r.value != "" && parsed.Pattern.MatchString(r.value)
		if triggered {
			reason = fmt.Sprintf("%q matches %s", r.value, parsed.Pattern)
		}

	case alert.Absent:
		if parsed.Duration == 0 {
			triggered = r.value == ""
		} else if ac.tracked() {
			n, _ := func(cronWatchID uint, since time.Time, tag string) (int, error) {
				since = ac.Limits.Clamp(since)
				n, err := ac.Crons.CountResults(cronWatchID, since, tag, true)
				return int(n), err
			}(ac.Watch.ID, time.Now().Add(-parsed.Duration), r.tag)
			triggered = n == 0
		} else {
			triggered = r.value == ""
		}
		reason = fmt.Sprintf("absent for %s", parsed.Duration)

	case alert.Rising:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", r.value)
			return 1
		}
		var baseline float64
		switch {
		case parsed.Duration > 0 && stats.Avg != nil:
			baseline = *stats.Avg
		case hasPrev:
			baseline = prevValue
		default:
			return 0
		}
		if baseline > 0 {
			pctChange := (current - baseline) / baseline
			triggered = pctChange >= parsed.PctChange
			reason = fmt.Sprintf("rose %.0f%% (from %.1f to %.1f)", pctChange*100, baseline, current)
		}

	case alert.Falling:
		if !isNumeric {
			fmt.Fprintf(stderr, "alert: non-numeric value %q\n", r.value)
			return 1
		}
		var baseline float64
		switch {
		case parsed.Duration > 0 && stats.Avg != nil:
			baseline = *stats.Avg
		case hasPrev:
			baseline = prevValue
		default:
			return 0
		}
		if baseline > 0 {
			pctChange := (baseline - current) / baseline
			triggered = pctChange >= parsed.PctChange
			reason = fmt.Sprintf("fell %.0f%% (from %.1f to %.1f)", pctChange*100, baseline, current)
		}
	}

	if !triggered {
		ac.closeIncident(ctx, r.tag)
		return 0
	}

	msg := parsed.Msg
	if msg == "" {
		prefix := "weft"
		if ac.Watch != nil {
			prefix = ac.Watch.Name
		}
		if r.tag != "" {
			prefix = prefix + "[" + r.tag + "]"
		}
		msg = prefix + ": " + reason
	}

	inc := ac.openIncident(ctx, r.tag)
	fmt.Fprintln(stderr, msg)
	return ac.notify(ctx, inc, parsed.Notify, r.tag, msg, stderr)
}

// dispatchIdentity returns the (watchName, owner) used by the Notifier
// for dedup-key construction. For ad-hoc shell runs a unique synthetic name
// ensures every invocation creates its own PagerDuty incident. When a tag
// is set, it's appended to the watch name so PagerDuty sees per-tag
// incidents collapse into per-tag dedup keys, not one watch-wide key.
func (ac *AlertContext) dispatchIdentity(tag string) (string, shape.Added) {
	if ac.Watch != nil {
		name := ac.Watch.Name
		// A namespaced cron is named <namespace>/<name>: two namespaces can
		// each have a `disk-use`, and this name keys the PagerDuty dedup.
		if ac.Watch.Namespace != 0 && ac.Sources != nil {
			if ns, ok, err := ac.Sources.NamespaceName(ac.Owner, ac.Watch.Namespace); err == nil && ok {
				name = ns + "/" + name
			}
		}
		if tag != "" {
			name = name + "[" + tag + "]"
		}
		return name, ac.Owner
	}
	return fmt.Sprintf("adhoc-%d", time.Now().UnixNano()), ac.Owner
}

func truncateAlertBody(s string) string {
	const max = 1024
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}

// evaluateIfMode runs the dispatch+incident-dedup half of an alert
// invocation given a triggered/not-triggered signal from --if-zero
// or --if-nonzero. Carries the same incident open/close behavior as
// evaluateAlertRow so a composite predicate watch behaves identically
// to a stdin-condition watch — alerts open when the predicate trips,
// close when it stops tripping.
func evaluateIfMode(ctx context.Context, triggered bool, parsed *alert.Args, ac *AlertContext, stderr io.Writer) int {
	if !triggered {
		ac.closeIncident(ctx, "")
		return 0
	}

	msg := parsed.Msg
	if msg == "" {
		prefix := "weft"
		if ac.Watch != nil {
			prefix = ac.Watch.Name
		}
		mode := "if-zero"
		if parsed.IfMode == alert.IfNonzero {
			mode = "if-nonzero"
		}
		msg = prefix + ": predicate composite (" + mode + ") fired"
	}

	inc := ac.openIncident(ctx, "")
	fmt.Fprintln(stderr, msg)
	return ac.notify(ctx, inc, parsed.Notify, "", msg, stderr)
}

// openIncident records that the alert holds on this tick and returns the
// cron's open incident for tag, opening one if needed. It returns nil for
// interactive runs, which have no cron and so no incidents.
func (ac *AlertContext) openIncident(ctx context.Context, tag string) *cron.Incident {
	if !ac.tracked() {
		return nil
	}
	inc, err := ac.Crons.OpenOrTouchIncident(ac.Watch.ID, tag)
	if err != nil {
		// Without the incident every tick would notify afresh; say so loudly
		// rather than silently double-paging.
		weftsdk.Infof(ctx, "alert: open incident watch=%d tag=%q: %v", ac.Watch.ID, tag, err)
		return nil
	}
	return inc
}

// closeIncident closes the cron's open incident for tag: the condition no
// longer holds. Closing sends nothing.
func (ac *AlertContext) closeIncident(ctx context.Context, tag string) {
	if !ac.tracked() {
		return
	}
	if err := ac.Crons.CloseIncident(ac.Watch.ID, tag); err != nil {
		weftsdk.Infof(ctx, "alert: close incident watch=%d tag=%q: %v", ac.Watch.ID, tag, err)
	}
}

// notify sends msg to the alias named by --notify and returns the alert's
// exit code. Within an incident, targets that already received it are
// skipped, and any that missed it (a failed send on an earlier tick) are
// tried again. Outside an incident (an interactive run) every send goes out.
func (ac *AlertContext) notify(ctx context.Context, inc *cron.Incident, alias, tag, msg string, stderr io.Writer) int {
	if alias == "" {
		return 0
	}
	if ac.Notifier == nil {
		if ac.Interactive {
			fmt.Fprintf(stderr, "alert: notifier unavailable — would dispatch to %q\n", alias)
		}
		return 0
	}
	var delivered map[string]bool
	if inc != nil {
		var err error
		if delivered, err = ac.Crons.IncidentDeliveries(inc.ID); err != nil {
			weftsdk.Infof(ctx, "alert: read deliveries incident=%s: %v", inc.ID, err)
		}
	}
	// Dispatch the formatted alert message (what the shell prints to
	// stderr), not the raw triggering row — Slack/PagerDuty users
	// want "watchname[tag]: 5.12 > 5.00", not "5.12 disk" on its own.
	watchName, owner := ac.dispatchIdentity(tag)
	sent, err := ac.Notifier.Send(ctx, owner, alias, watchName, truncateAlertBody(msg), delivered)
	if inc != nil {
		if rerr := ac.Crons.RecordDeliveries(inc.ID, sent); rerr != nil {
			weftsdk.Infof(ctx, "alert: record deliveries incident=%s: %v", inc.ID, rerr)
		}
	}
	if err != nil {
		if errors.Is(err, ErrDeliveryTimeout) {
			noteDeliveryTimeout(ctx)
		}
		fmt.Fprintf(stderr, "alert: dispatch to %q failed: %v\n", alias, err)
		return 1
	}
	return 0
}
