package coordinator

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/alert"
	"github.com/graybeardcorp/weft/internal/weftlog"
	"github.com/graybeardcorp/weft/store"
	"gorm.io/gorm"
)

// NotifyDispatcher is the subset of Notifier the alert stage calls.
// Narrowed to an interface so tests can intercept Send without a live
// HTTP endpoint. The concrete *Notifier satisfies it.
type NotifyDispatcher interface {
	Send(ctx context.Context, orgID store.OrgID, aliasName, watchName, body string) error
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
// condition. In single-row mode (no -k), stdin is one scalar per tick. In
// multi-row mode (-k set), each non-blank line of stdin is evaluated
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

	condStr := strings.Join(args, " ")

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
		return evaluateIfMode(ctx, fired, parsed, condStr, ac, stderr)
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
		rc := evaluateAlertRow(ctx, r, parsed, condStr, ac, stderr)
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
// an empty value/tag (evaluated as non-numeric downstream). When -k uses
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
func evaluateAlertRow(ctx context.Context, r alertRow, parsed *alert.Args, condStr string, ac *AlertContext, stderr io.Writer) int {
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
			weftlog.Infof(ctx, "alert: ListCronResults watch=%d tag=%q: %v", ac.Watch.ID, r.tag, err)
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
		// Still close any open incident for this (watch, condition, tag) —
		// condition cleared.
		if ac.Watch != nil && ac.Store != nil {
			checkIncident(ctx, ac.Store, ac.Watch.ID, condStr, r.tag, false)
		}
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

	shouldDispatch := true
	if ac.Watch != nil && ac.Store != nil {
		shouldDispatch = checkIncident(ctx, ac.Store, ac.Watch.ID, condStr, r.tag, true)
	}

	fmt.Fprintln(stderr, msg)

	if shouldDispatch && parsed.Notify != "" && ac.Notifier != nil {
		// Dispatch the formatted alert message (what the shell prints to
		// stderr), not the raw triggering row — Slack/PagerDuty users
		// want "watchname[tag]: 5.12 > 5.00", not "5.12 disk" on its own.
		body := truncateAlertBody(msg)
		watchName, orgID := ac.dispatchIdentity(r.tag)
		if err := ac.Notifier.Send(ctx, orgID, parsed.Notify, watchName, body); err != nil {
			fmt.Fprintf(stderr, "alert: dispatch to %q failed: %v\n", parsed.Notify, err)
			return 1
		}
	} else if parsed.Notify != "" && ac.Notifier == nil && ac.Interactive {
		fmt.Fprintf(stderr, "alert: notifier unavailable — would dispatch to %q\n", parsed.Notify)
	}
	return 0
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
func evaluateIfMode(ctx context.Context, triggered bool, parsed *alert.Args, condStr string, ac *AlertContext, stderr io.Writer) int {
	if !triggered {
		if ac.Watch != nil && ac.Store != nil {
			checkIncident(ctx, ac.Store, ac.Watch.ID, condStr, "", false)
		}
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

	shouldDispatch := true
	if ac.Watch != nil && ac.Store != nil {
		shouldDispatch = checkIncident(ctx, ac.Store, ac.Watch.ID, condStr, "", true)
	}

	fmt.Fprintln(stderr, msg)

	if shouldDispatch && parsed.Notify != "" && ac.Notifier != nil {
		body := truncateAlertBody(msg)
		watchName, orgID := ac.dispatchIdentity("")
		if err := ac.Notifier.Send(ctx, orgID, parsed.Notify, watchName, body); err != nil {
			fmt.Fprintf(stderr, "alert: dispatch to %q failed: %v\n", parsed.Notify, err)
			return 1
		}
	} else if parsed.Notify != "" && ac.Notifier == nil && ac.Interactive {
		fmt.Fprintf(stderr, "alert: notifier unavailable — would dispatch to %q\n", parsed.Notify)
	}
	return 0
}

// checkIncident manages per-tag incident dedup. Returns true if this is a
// NEW incident and dispatch should fire; false if an incident is already
// open (suppress) or the condition cleared (close).
func checkIncident(ctx context.Context, s *store.Store, watchID uint, condition, tag string, triggered bool) bool {
	existing, err := func() (*store.AlertIncident, error) {
		var inc store.AlertIncident
		err := s.DB().Where("cron_watch_id = ? AND condition = ? AND tag = ? AND closed_at IS NULL",
			watchID, condition, tag).First(&inc).Error
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return &inc, nil
	}()
	if err != nil {
		// A swallow here would silently double-fire on every tick;
		// log loudly so we can spot DB-side regressions.
		weftlog.Infof(ctx, "alert: OpenIncident watch=%d condition=%q tag=%q: %v", watchID, condition, tag, err)
	}
	if triggered {
		if existing != nil {
			now := time.Now()
			s.DB().Model(&store.AlertIncident{}).Where("id = ?", existing.ID).Updates(map[string]any{
				"last_seen_at": now,
				"updated_at":   now,
			})
			return false
		}
		now := time.Now()
		inc := store.AlertIncident{
			ID:          uuid.New().String(),
			CronWatchID: watchID,
			Condition:   condition,
			Tag:         tag,
			OpenedAt:    now,
			LastSeenAt:  now,
			NotifiedAt:  &now,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		s.DB().Create(&inc)
		return true
	}
	if existing != nil {
		now := time.Now()
		s.DB().Model(&store.AlertIncident{}).Where("id = ?", existing.ID).Updates(map[string]any{
			"closed_at":  &now,
			"updated_at": now,
		})
	}
	return false
}
