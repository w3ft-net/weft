package coordinator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/alert"
	"github.com/graybeardcorp/weft/store"
)

// NotifyDispatcher is the subset of Notifier the alert stage calls.
// Narrowed to an interface so tests can intercept Send without a live
// HTTP endpoint. The concrete *Notifier satisfies it.
type NotifyDispatcher interface {
	Send(ctx context.Context, orgID store.OrgID, aliasName, watchName, body string, delivered map[string]bool) (sent []string, err error)
}

// AlertContext provides the alert command with access to result history,
// incident dedup, and the Notifier. Watch is nil for interactive runs,
// which skips (watchID, condition, tag) dedup but still dispatches if
// --notify is supplied — useful for testing aliases from the shell.
type AlertContext struct {
	Store       *store.Store
	Watch       *store.CronWatch
	Notifier    NotifyDispatcher
	OrgID       store.OrgID
	Interactive bool
}

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
		if v, err := shellcore.ResolveField(parsed.Key, trim); err == nil {
			r.value = v
		}
		if !parsed.Tag.IsZero() {
			if t, err := shellcore.ResolveField(parsed.Tag, trim); err == nil {
				r.tag = t
			}
		}
		rows = append(rows, r)
	}
	return rows
}

// evaluateAlertRow runs condition evaluation + dedup + dispatch for one row.
// Returns 1 on a hard parse/eval error that the caller should propagate as
// the exit code.
func evaluateAlertRow(ctx context.Context, r alertRow, parsed *alert.Args, ac *AlertContext, stderr io.Writer) int {
	current, numErr := strconv.ParseFloat(r.value, 64)
	isNumeric := numErr == nil

	// Window aggregates (per-tag when we have one).
	var stats store.CronAggStats
	if parsed.Duration > 0 && ac.Store != nil && ac.Watch != nil {
		stats, _ = func(orgID store.OrgID, cronWatchID uint, since time.Time, tag string) (store.CronAggStats, error) {
			since = store.ClampRetention(ac.Store.TierForOrg(orgID), since)
			gran := store.PickGranularity(time.Since(since))

			var row struct {
				Avg     *float64
				Min     *float64
				Max     *float64
				Samples int
			}
			query := `
				SELECT SUM(sum) / NULLIF(SUM(count), 0) AS avg,
				       MIN(min)                         AS min,
				       MAX(max)                         AS max,
				       COALESCE(SUM(count), 0)          AS samples
				FROM cron_results_all
				WHERE granularity = ?
				  AND cron_watch_id = ?
				  AND bucket_start >= ?`
			args := []any{gran, cronWatchID, since}
			if tag != "" {
				query += " AND tag = ?"
				args = append(args, tag)
			}
			err := ac.Store.DB().Raw(query, args...).Scan(&row).Error
			return store.CronAggStats{Avg: row.Avg, Min: row.Min, Max: row.Max, Samples: row.Samples}, err
		}(ac.OrgID, ac.Watch.ID, time.Now().Add(-parsed.Duration), r.tag)
	}

	// Previous tick's value for this same tag (changed/rising/falling).
	var prevValue float64
	var prevStdout string
	var hasPrev bool
	if ac.Store != nil && ac.Watch != nil {
		results, err := ac.Store.ListCronResults(ac.Watch.ID, 2, r.tag)
		if err != nil {
			weftsdk.Infof(ctx, "alert: ListCronResults watch=%d tag=%q: %v", ac.Watch.ID, r.tag, err)
		}
		if len(results) >= 2 {
			prevStdout = strings.TrimSpace(results[1].Stdout)
			prevValueStr := prevStdout
			if results[1].Value != nil {
				prevValueStr = strconv.FormatFloat(*results[1].Value, 'f', -1, 64)
			}
			if v, err := strconv.ParseFloat(prevValueStr, 64); err == nil {
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
		if hasPrev || prevStdout != "" {
			triggered = r.value != prevStdout
			if triggered {
				reason = fmt.Sprintf("changed from %q to %q", prevStdout, r.value)
			}
		}

	case alert.Absent:
		if parsed.Duration == 0 {
			triggered = r.value == ""
		} else if ac.Store != nil && ac.Watch != nil {
			n, _ := func(orgID store.OrgID, cronWatchID uint, since time.Time, tag string) (int, error) {
				since = store.ClampRetention(ac.Store.TierForOrg(orgID), since)
				var n int64
				query := `
					SELECT COUNT(*) FROM cron_results
					WHERE cron_watch_id = ? AND ran_at >= ? AND stdout <> ''`
				args := []any{cronWatchID, since}
				if tag != "" {
					query += " AND tag = ?"
					args = append(args, tag)
				}
				err := ac.Store.DB().Raw(query, args...).Scan(&n).Error
				return int(n), err
			}(ac.OrgID, ac.Watch.ID, time.Now().Add(-parsed.Duration), r.tag)
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

// dispatchIdentity returns the (watchName, orgID) used by the Notifier
// for dedup-key construction. For ad-hoc shell runs a unique synthetic name
// ensures every invocation creates its own PagerDuty incident. When a tag
// is set, it's appended to the watch name so PagerDuty sees per-tag
// incidents collapse into per-tag dedup keys, not one watch-wide key.
func (ac *AlertContext) dispatchIdentity(tag string) (string, store.OrgID) {
	if ac.Watch != nil {
		name := ac.Watch.Name
		if tag != "" {
			name = name + "[" + tag + "]"
		}
		return name, ac.Watch.OrgID
	}
	return fmt.Sprintf("adhoc-%d", time.Now().UnixNano()), ac.OrgID
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
func (ac *AlertContext) openIncident(ctx context.Context, tag string) *store.AlertIncident {
	if ac.Watch == nil || ac.Store == nil {
		return nil
	}
	inc, err := ac.Store.OpenOrTouchIncident(ac.Watch.ID, tag)
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
	if ac.Watch == nil || ac.Store == nil {
		return
	}
	if err := ac.Store.CloseIncident(ac.Watch.ID, tag); err != nil {
		weftsdk.Infof(ctx, "alert: close incident watch=%d tag=%q: %v", ac.Watch.ID, tag, err)
	}
}

// notify sends msg to the alias named by --notify and returns the alert's
// exit code. Within an incident, targets that already received it are
// skipped, and any that missed it (a failed send on an earlier tick) are
// tried again. Outside an incident (an interactive run) every send goes out.
func (ac *AlertContext) notify(ctx context.Context, inc *store.AlertIncident, alias, tag, msg string, stderr io.Writer) int {
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
		if delivered, err = ac.Store.IncidentDeliveries(inc.ID); err != nil {
			weftsdk.Infof(ctx, "alert: read deliveries incident=%s: %v", inc.ID, err)
		}
	}
	// Dispatch the formatted alert message (what the shell prints to
	// stderr), not the raw triggering row — Slack/PagerDuty users
	// want "watchname[tag]: 5.12 > 5.00", not "5.12 disk" on its own.
	watchName, orgID := ac.dispatchIdentity(tag)
	sent, err := ac.Notifier.Send(ctx, orgID, alias, watchName, truncateAlertBody(msg), delivered)
	if inc != nil {
		if rerr := ac.Store.RecordDeliveries(inc.ID, sent); rerr != nil {
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
