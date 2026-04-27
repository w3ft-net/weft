package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kevlarcon/warp/pkg/bash"
	"github.com/kevlarcon/warp/pkg/shellcore"
	luaparse "github.com/yuin/gopher-lua/parse"
	"gorm.io/gorm"
)

// ParseScheduleInterval parses a schedule string into a duration.
// Supports "every <duration>" (e.g., "every 1m") and basic cron
// shorthand "*/N * * * *" (every N minutes). Returns 5m for
// unrecognized formats.
func ParseScheduleInterval(schedule string) time.Duration {
	if strings.HasPrefix(schedule, "every ") {
		if d, err := time.ParseDuration(strings.TrimPrefix(schedule, "every ")); err == nil {
			return d
		}
	}

	if strings.HasPrefix(schedule, "*/") {
		parts := strings.Fields(schedule)
		if len(parts) >= 1 {
			nStr := strings.TrimPrefix(parts[0], "*/")
			n := 0
			for _, ch := range nStr {
				if ch >= '0' && ch <= '9' {
					n = n*10 + int(ch-'0')
				} else {
					break
				}
			}
			if n > 0 {
				return time.Duration(n) * time.Minute
			}
		}
	}

	return 5 * time.Minute
}

// ShellFunction is a user-defined function stored in the virtual filesystem.
// Language is detected from shebang at write time: "bash" (default) or "lua".
type ShellFunction struct {
	gorm.Model
	CustomerID      uint    `gorm:"not null;uniqueIndex:idx_func_customer_name"`
	Name            string  `gorm:"not null;uniqueIndex:idx_func_customer_name"`
	Source          string  `gorm:"type:text;not null"`
	Language        string  `gorm:"not null;default:bash"` // "bash" or "lua"
	ContentHash     string  `gorm:"column:content_hash"`
	Message         string  `gorm:"column:message"`
	CreatedByUserID *string `gorm:"column:created_by_user_id"`
}

func (s *Store) SaveShellFunction(customerID uint, userID, name, source string) error {
	lang, cleanSource := detectLanguage(source)
	if err := validateSource(lang, cleanSource); err != nil {
		return err
	}
	hash := computeShellFunctionHash(cleanSource)

	return s.db.Transaction(func(tx *gorm.DB) error {
		// Set the per-transaction GUC the version-history trigger
		// reads to populate captured_by_user_id. Empty userID is
		// stored as '' — the trigger collapses '' to NULL. We
		// always set explicitly (rather than skipping for empty)
		// because tests wrap calls in an outer BeginTest tx, and
		// inner Transaction()s become savepoints; SET LOCAL is
		// scoped to the outer tx, so a previous call's value
		// would otherwise bleed in.
		if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", userID).Error; err != nil {
			return err
		}

		var fn ShellFunction
		result := tx.Where("customer_id = ? AND name = ?", customerID, name).First(&fn)
		if result.Error == gorm.ErrRecordNotFound {
			row := &ShellFunction{
				CustomerID: customerID, Name: name,
				Source: cleanSource, Language: lang,
				ContentHash: hash,
			}
			if userID != "" {
				row.CreatedByUserID = &userID
			}
			return tx.Create(row).Error
		}
		// Existing rows keep their original creator on overwrite —
		// only source / language / hash change. Editing isn't authoring.
		return tx.Model(&fn).Updates(map[string]any{
			"source":       cleanSource,
			"language":     lang,
			"content_hash": hash,
		}).Error
	})
}

// validateSource rejects syntactically-invalid /bin scripts at save time.
// Catching parse errors here surfaces them to the user (in vi's status line
// on :w) instead of letting an unparseable script silently persist and
// then 500 at invocation time.
//
// Bash scripts additionally must satisfy a set of shape rules that would
// otherwise produce confusing runtime behaviour:
//   - validateAlertPosition — alerts only at the terminal position.
//   - validateNoFollowMode — `tail -f` is incompatible with the
//     body-runs-to-completion execution model for saved scripts.
//   - validateNoRangeBuiltin — `range` adjusts the request-scoped
//     time window, which a saved script cannot scope properly (its
//     effects would leak to subsequent interior queries in
//     unexpected ways).
func validateSource(lang, source string) error {
	switch lang {
	case "bash":
		script, err := bash.Parse(source)
		if err != nil {
			return fmt.Errorf("bash syntax error: %w", err)
		}
		if err := validateAlertPosition(script); err != nil {
			return err
		}
		if err := validateNoFollowMode(script); err != nil {
			return err
		}
		if err := validateNoRangeBuiltin(script); err != nil {
			return err
		}
	case "lua":
		if _, err := luaparse.Parse(strings.NewReader(source), "source"); err != nil {
			return fmt.Errorf("lua syntax error: %w", err)
		}
	}
	return nil
}

// validateAlertPosition enforces that any `alert` stage in a /bin
// script body appears only as the terminal stage of the body's final
// top-level list, and that list is a bare pipeline (not inside
// if/for/while/case/subshell/group/function). Any other placement is
// rejected with a user-facing error that names the offending location.
//
// Rationale: a body that invokes `alert` mid-flow or inside a loop
// fires the alert N times per script invocation, typically N > the
// user's intent. Constraining alert to "the last thing the body
// does" matches how interactive pipelines use alerts today and
// matches the outer-watch model (one alert dispatch per watch run).
func validateAlertPosition(script *bash.Script) error {
	if script == nil || len(script.Commands) == 0 {
		return nil
	}
	// Find every `alert` SimpleCommand anywhere in the tree, along
	// with whether it sits at the allowed position.
	lastIdx := len(script.Commands) - 1
	allowed := make(map[*bash.SimpleCommand]bool)
	if sc := terminalAlertOf(script.Commands[lastIdx]); sc != nil {
		allowed[sc] = true
	}

	var offender *bash.SimpleCommand
	bash.Walk(script, func(n bash.Node) bool {
		if offender != nil {
			return false
		}
		sc, ok := n.(*bash.SimpleCommand)
		if !ok || len(sc.Words) == 0 {
			return true
		}
		if sc.Words[0].Literal() != "alert" {
			return true
		}
		if !allowed[sc] {
			offender = sc
		}
		return true
	})
	if offender != nil {
		return fmt.Errorf("alert may only appear as the final stage of a /bin script body, not in a leading list, loop body, conditional, or nested command")
	}
	return nil
}

// validateNoFollowMode rejects scripts that use `tail -f` anywhere in
// the body. Follow mode streams indefinitely — it has no natural
// completion point — which conflicts with the body-runs-to-completion
// execution model for saved scripts (where a caller, cron, or HTTP
// request is waiting for a final stdout). Allowing it would mean
// either the script hangs forever or the first ERROR row kills the
// whole stream; neither is what the user expects.
//
// If the user genuinely wants to follow logs, `tail -f` works at the
// interactive shell prompt where there is no completion requirement.
func validateNoFollowMode(script *bash.Script) error {
	if script == nil {
		return nil
	}
	var found *bash.SimpleCommand
	bash.Walk(script, func(n bash.Node) bool {
		if found != nil {
			return false
		}
		sc, ok := n.(*bash.SimpleCommand)
		if !ok || len(sc.Words) == 0 {
			return true
		}
		if sc.Words[0].Literal() != "tail" {
			return true
		}
		for _, w := range sc.Words[1:] {
			lit := w.Literal()
			if lit == "-f" || lit == "--follow" || strings.HasPrefix(lit, "-f") {
				found = sc
				return false
			}
		}
		return true
	})
	if found != nil {
		return fmt.Errorf("tail -f is not allowed in saved scripts (follow mode streams indefinitely and has no completion point); use `tail -f` interactively at the shell prompt instead")
	}
	return nil
}

// validateNoRangeBuiltin rejects scripts that invoke the `range`
// shell builtin. `range` adjusts the request's time window in-place;
// inside a saved script its effect would silently leak into every
// interior query the script fires afterwards, producing windows the
// caller did not ask for. Saved scripts inherit `tr` from the
// request that invoked them; users who need a different window
// should invoke the script from a shell session where they've
// already set the window, or use explicit time predicates in their
// awk/grep stages.
func validateNoRangeBuiltin(script *bash.Script) error {
	if script == nil {
		return nil
	}
	var found *bash.SimpleCommand
	bash.Walk(script, func(n bash.Node) bool {
		if found != nil {
			return false
		}
		sc, ok := n.(*bash.SimpleCommand)
		if !ok || len(sc.Words) == 0 {
			return true
		}
		if sc.Words[0].Literal() == "range" {
			found = sc
			return false
		}
		return true
	})
	if found != nil {
		return fmt.Errorf("range is not allowed in saved scripts (it modifies the request-scoped time window and would leak into interior queries); set the window at the call site or use explicit time predicates inside the script")
	}
	return nil
}

// terminalAlertOf returns the `alert` SimpleCommand that is the last
// stage of list's first (and only allowed) pipeline, iff list is a
// bare single-pipeline list (no ListCont continuations). Any other
// shape returns nil, which means no alert in that list is "allowed"
// — including alerts sitting at the end of a compound list like
// `foo && bar | alert`, where the alert's semantics are too
// short-circuit-dependent to reason about cleanly.
func terminalAlertOf(list *bash.List) *bash.SimpleCommand {
	if list == nil || list.First == nil {
		return nil
	}
	if len(list.Rest) != 0 {
		return nil
	}
	pipe := list.First
	if len(pipe.Commands) == 0 {
		return nil
	}
	last := pipe.Commands[len(pipe.Commands)-1]
	sc, ok := last.(*bash.SimpleCommand)
	if !ok || len(sc.Words) == 0 {
		return nil
	}
	if sc.Words[0].Literal() != "alert" {
		return nil
	}
	return sc
}

// detectLanguage checks for a Lua shebang and returns the language
// and source with the shebang stripped.
func detectLanguage(source string) (lang, cleanSource string) {
	firstLine, rest, _ := strings.Cut(source, "\n")
	trimmed := strings.TrimSpace(firstLine)
	if strings.HasPrefix(trimmed, "#!") && strings.Contains(trimmed, "lua") {
		return "lua", rest
	}
	return "bash", source
}

func (s *Store) GetShellFunction(customerID uint, name string) (*ShellFunction, error) {
	var fn ShellFunction
	err := s.db.Where("customer_id = ? AND name = ?", customerID, name).First(&fn).Error
	if err != nil {
		return nil, err
	}
	return &fn, nil
}

func (s *Store) ListShellFunctions(customerID uint) ([]ShellFunction, error) {
	var fns []ShellFunction
	err := s.db.Where("customer_id = ?", customerID).Order("name").Find(&fns).Error
	return fns, err
}

func (s *Store) DeleteShellFunction(customerID uint, name string) error {
	return s.db.Where("customer_id = ? AND name = ?", customerID, name).Delete(&ShellFunction{}).Error
}

// --- Cron Watches (/cron.d/) ---

// CronWatch is a scheduled pipeline run. AlertKey and AlertTag are the
// pre-parsed -k and -t selectors from the pipeline's `| alert` stage;
// nil means single-row mode (no extraction). They're populated by
// SaveCronWatch so the executor doesn't reparse the pipeline every tick.
// Stored as text — either a 1-indexed column number ("3") or a
// dot-prefixed JSON path (".foo.bar"); see shellcore.FieldSpec.
type CronWatch struct {
	gorm.Model
	CustomerID uint   `gorm:"not null;uniqueIndex:idx_cron_customer_name"`
	Name       string `gorm:"not null;uniqueIndex:idx_cron_customer_name"`
	Schedule   string `gorm:"not null"`        // "*/5 * * * *" or "every 1m"
	Pipeline   string `gorm:"type:text;not null"`
	Enabled    bool   `gorm:"not null;default:true"`
	LastRunAt  *time.Time
	LastStatus string                           // "ok", "error", "stderr"
	AlertKey   *string `gorm:"column:alert_key"`
	AlertTag   *string `gorm:"column:alert_tag"`
	// AlertCondition + AlertThreshold are the dashboard projection of
	// the `alert` stage's numeric cutoff. Populated only for
	// above/below/eq/neq — conditions without a single absolute
	// threshold (rising/falling/changed/absent/any) leave both NULL.
	AlertCondition  *string  `gorm:"column:alert_condition"`
	AlertThreshold *float64 `gorm:"column:alert_threshold"`
	ContentHash     string   `gorm:"column:content_hash"`
	Message         string   `gorm:"column:message"`
	CreatedByUserID *string  `gorm:"column:created_by_user_id"`
}

// computeCronWatchHash returns the canonical content hash for a
// cron watch — SHA-256 over schedule + "\n" + pipeline. Matches
// the Postgres backfill expression in migration 036.
func computeCronWatchHash(schedule, pipeline string) string {
	h := sha256.Sum256([]byte(schedule + "\n" + pipeline))
	return hex.EncodeToString(h[:])
}

func (s *Store) SaveCronWatch(customerID uint, userID, name, schedule, pipeline string) error {
	compiled, err := CompileWatchPipeline(pipeline)
	if err != nil {
		return err
	}
	hash := computeCronWatchHash(schedule, pipeline)

	return s.db.Transaction(func(tx *gorm.DB) error {
		// See SaveShellFunction for why we always set, even for empty.
		if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", userID).Error; err != nil {
			return err
		}
		var w CronWatch
		result := tx.Where("customer_id = ? AND name = ?", customerID, name).First(&w)
		if result.Error == gorm.ErrRecordNotFound {
			row := &CronWatch{
				CustomerID:     customerID,
				Name:           name,
				Schedule:       schedule,
				Pipeline:       pipeline,
				Enabled:        true,
				AlertKey:       compiled.AlertKey,
				AlertTag:       compiled.AlertTag,
				AlertCondition: compiled.AlertCondition,
				AlertThreshold: compiled.AlertThreshold,
				ContentHash:    hash,
			}
			if userID != "" {
				row.CreatedByUserID = &userID
			}
			return tx.Create(row).Error
		}
		// Existing watches keep their original creator on overwrite.
		return tx.Model(&w).Updates(map[string]any{
			"schedule":        schedule,
			"pipeline":        pipeline,
			"content_hash":    hash,
			"alert_key":       compiled.AlertKey,
			"alert_tag":       compiled.AlertTag,
			"alert_condition": compiled.AlertCondition,
			"alert_threshold": compiled.AlertThreshold,
		}).Error
	})
}

// CompiledWatch holds the pre-parsed bits of an `alert` stage that we want
// to stamp onto the watch row so the executor doesn't re-parse on every
// tick. AlertKey/Tag are runtime-relevant (they shape how cron_results
// gets written); AlertCondition/Threshold are dashboard projections for
// threshold-line rendering and are only populated for conditions with a
// single numeric cutoff (above, below, eq, neq).
type CompiledWatch struct {
	// AlertKey/AlertTag are stamped as the FieldSpec source form
	// ("3" or ".foo.bar"); the executor re-parses them via
	// shellcore.ParseFieldSpec to recover the FieldSpec.
	AlertKey       *string
	AlertTag       *string
	AlertCondition *string
	AlertThreshold *float64
}

// CompileWatchPipeline validates pipeline-shape invariants and returns the
// compiled extractor positions for the (at most one) `alert` stage.
//
// Validation rules:
//   - The pipeline must parse as bash.
//   - `alert` may appear at most once in the pipeline. The single stage is
//     the sole incident-dedup boundary per watch run.
//   - `-k` / `-t` must be a positive integer or a `.`-prefixed JSON path
//     if present (enforced by shellcore.ParseAlertArgs).
//
// The AST walk catches alerts nested in command substitutions, compound
// commands, and function bodies so the only way past validation is alias
// or variable obfuscation.
func CompileWatchPipeline(pipeline string) (CompiledWatch, error) {
	var out CompiledWatch
	script, err := bash.Parse(pipeline)
	if err != nil {
		return out, fmt.Errorf("parse: %w", err)
	}
	var alertCmds []*bash.SimpleCommand
	bash.Walk(script, func(n bash.Node) bool {
		if sc, ok := n.(*bash.SimpleCommand); ok {
			if len(sc.Words) > 0 && sc.Words[0].Literal() == "alert" {
				alertCmds = append(alertCmds, sc)
			}
		}
		return true
	})
	if len(alertCmds) > 1 {
		return out, fmt.Errorf("alert may only appear once in a pipeline (found %d)", len(alertCmds))
	}
	if len(alertCmds) == 1 {
		argv := make([]string, 0, len(alertCmds[0].Words)-1)
		for _, w := range alertCmds[0].Words[1:] {
			argv = append(argv, w.Literal())
		}
		parsed, perr := shellcore.ParseAlertArgs(argv)
		if perr != nil {
			return out, fmt.Errorf("alert: %w", perr)
		}
		if !parsed.Key.IsZero() {
			s := parsed.Key.String()
			out.AlertKey = &s
		}
		if !parsed.Tag.IsZero() {
			s := parsed.Tag.String()
			out.AlertTag = &s
		}
		// Stamp the condition + numeric threshold for dashboard line
		// rendering. Only above/below/eq/neq carry a single absolute
		// threshold — other conditions leave both fields nil.
		switch parsed.Condition {
		case shellcore.AlertAbove, shellcore.AlertBelow, shellcore.AlertEq, shellcore.AlertNeq:
			cond := parsed.Condition.String()
			thr := parsed.Threshold
			out.AlertCondition = &cond
			out.AlertThreshold = &thr
		}
	}
	return out, nil
}

// ValidateWatchPipeline is the error-only form of CompileWatchPipeline for
// callers that don't need the compiled extractors.
func ValidateWatchPipeline(pipeline string) error {
	_, err := CompileWatchPipeline(pipeline)
	return err
}

func (s *Store) GetCronWatch(customerID uint, name string) (*CronWatch, error) {
	var w CronWatch
	err := s.db.Where("customer_id = ? AND name = ?", customerID, name).First(&w).Error
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) ListCronWatches(customerID uint) ([]CronWatch, error) {
	var watches []CronWatch
	err := s.db.Where("customer_id = ?", customerID).Order("name").Find(&watches).Error
	return watches, err
}

func (s *Store) ListEnabledCronWatches() ([]CronWatch, error) {
	var watches []CronWatch
	err := s.db.Where("enabled = ?", true).Find(&watches).Error
	return watches, err
}

func (s *Store) DeleteCronWatch(customerID uint, name string) error {
	return s.db.Where("customer_id = ? AND name = ?", customerID, name).Delete(&CronWatch{}).Error
}

// CountCronWatches returns the number of cron watches for a customer.
func (s *Store) CountCronWatches(customerID uint) (int, error) {
	var count int64
	err := s.db.Model(&CronWatch{}).Where("customer_id = ?", customerID).Count(&count).Error
	return int(count), err
}

func (s *Store) UpdateCronWatchRun(id uint, status string) error {
	now := time.Now()
	return s.db.Model(&CronWatch{}).Where("id = ?", id).Updates(map[string]any{
		"last_run_at": &now,
		"last_status": status,
	}).Error
}

// --- Cron Results ---

// CronResult stores the output of a completed cron watch run. Value holds the
// numeric form of Stdout (or, in multi-row mode, the extracted field -k of
// one output line) when it parses cleanly as a float; NULL otherwise. Tag
// labels the row in multi-row mode ("" for single-row mode, the default).
// Aggregations read Value directly in SQL and ignore the NULLs; tag-scoped
// aggregates filter by tag.
type CronResult struct {
	gorm.Model
	CronWatchID uint      `gorm:"not null;index"`
	RanAt       time.Time `gorm:"not null"`
	Stdout      string    `gorm:"type:text"`
	Stderr      string    `gorm:"type:text"`
	ExitCode    int
	DurationMs  int64
	Value       *float64 `gorm:"type:double precision"`
	Tag         string   `gorm:"type:text;not null;default:''"`
}

// SaveCronResult persists a single result row. Parses Stdout as a float into
// Value when Value is unset and Stdout trims cleanly; otherwise leaves Value
// as nil. Use SaveCronResults for the multi-row case.
func (s *Store) SaveCronResult(result *CronResult) error {
	if result.Value == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(result.Stdout), 64); err == nil {
			result.Value = &v
		}
	}
	return s.db.Create(result).Error
}

// SaveCronResults persists a batch of rows in one insert. Each row keeps
// whatever Value/Tag the caller set; no auto-parse like SaveCronResult,
// because in multi-row mode the executor has already done per-line
// extraction and an empty slice is a legal no-op.
func (s *Store) SaveCronResults(results []CronResult) error {
	if len(results) == 0 {
		return nil
	}
	return s.db.Create(&results).Error
}

// ListCronResults returns the most recent rows for a watch. If tag is
// non-empty, the scan is filtered to that tag; pass "" to get every row
// regardless of tag (used by single-row / legacy callers).
func (s *Store) ListCronResults(cronWatchID uint, limit int, tag string) ([]CronResult, error) {
	var results []CronResult
	q := s.db.Where("cron_watch_id = ?", cronWatchID)
	if tag != "" {
		q = q.Where("tag = ?", tag)
	}
	err := q.Order("ran_at DESC").Limit(limit).Find(&results).Error
	return results, err
}

// CronAggStats is the one-row result of an aggregation over a duration window.
// Avg/Min/Max are nil when Samples is 0 (no numeric rows in the window).
type CronAggStats struct {
	Avg     *float64
	Min     *float64
	Max     *float64
	Samples int
}

// CronAggregate returns avg/min/max/sample-count of cron_results.value for a
// watch over [since, now), optionally scoped to a single tag. Runs as a
// single SQL aggregate — alert uses this in place of loading rows and
// reducing in Go. Pass tag="" for single-row mode (which stores tag='' by
// default, so the filter still matches) or for cross-tag aggregates.
//
// Routes to the coarsest rollup tier whose data covers the requested
// window — raw (0–7d), hourly (7–14d), 4h (14–35d), daily (>35d). Also
// clamps `since` up to (now − LogRetention(tier)) so the API never
// returns data past the caller's plan retention even while it sits on
// disk. See docs/plans/retention-enforcement.md.
func (s *Store) CronAggregate(customerID, cronWatchID uint, since time.Time, tag string) (CronAggStats, error) {
	since = clampRetention(s.TierForCustomer(customerID), since)
	gran := pickGranularity(time.Since(since))

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
	err := s.db.Raw(query, args...).Scan(&row).Error
	return CronAggStats{Avg: row.Avg, Min: row.Min, Max: row.Max, Samples: row.Samples}, err
}

// CronBucket is one row of a (raw or rolled-up) series. For raw rows
// Count is always 1 and {Sum, Min, Max} all equal the single Value.
// For rollup rows they preserve exact avg/min/max/stddev through any
// subsequent merging. BucketStart is the left edge of the bucket (or
// the raw row's timestamp); Tag is "" for single-row watches.
type CronBucket struct {
	BucketStart time.Time `gorm:"column:bucket_start"`
	Tag         string
	Count       int64
	Sum         float64
	SumSq       float64 `gorm:"column:sum_sq"`
	Min         float64
	Max         float64
}

// QueryCronSeries returns one bucket per (tag, bucket_start) over
// [since, now) at the coarsest granularity whose retention covers the
// requested window. Powers the /watch/:name detail page — the chart
// draws one point per bucket (avg = sum/count), and rollup rows also
// carry min/max so the UI can shade a band when granularity != raw.
//
// `since` is clamped to (now − LogRetention(tier)) for the caller's
// plan; rows on disk past that cap are invisible to the API.
func (s *Store) QueryCronSeries(customerID, cronWatchID uint, since time.Time) ([]CronBucket, string, error) {
	since = clampRetention(s.TierForCustomer(customerID), since)
	gran := pickGranularity(time.Since(since))

	var rows []CronBucket
	err := s.db.Raw(`
		SELECT bucket_start, tag, count, sum, sum_sq, min, max
		FROM cron_results_all
		WHERE granularity = ?
		  AND cron_watch_id = ?
		  AND bucket_start >= ?
		ORDER BY bucket_start ASC`,
		gran, cronWatchID, since,
	).Scan(&rows).Error
	return rows, gran, err
}

// CronNonEmptyCount returns how many rows in [since, now) had non-empty
// stdout, optionally scoped to a tag. Used by `alert absent --duration
// <dur>` to tell whether any data arrived in the window.
//
// Raw-only: rollups don't carry stdout. Windows past the raw retention
// (7d) will return 0 regardless of actual history — if you need to ask
// "was there ever any output in the last 30 days" you're outside what
// the `absent` alert is designed for.
func (s *Store) CronNonEmptyCount(customerID, cronWatchID uint, since time.Time, tag string) (int, error) {
	since = clampRetention(s.TierForCustomer(customerID), since)
	var n int64
	query := `
		SELECT COUNT(*) FROM cron_results
		WHERE cron_watch_id = ? AND ran_at >= ? AND stdout <> ''`
	args := []any{cronWatchID, since}
	if tag != "" {
		query += " AND tag = ?"
		args = append(args, tag)
	}
	err := s.db.Raw(query, args...).Scan(&n).Error
	return int(n), err
}

// QueryCronResults returns results for a named watch within a time range,
// scoped to a customer. Used by weft.* functions in /bin/ scripts.
//
// Raw-only — the full CronResult row (with stdout/stderr/exit_code)
// only exists in the unrolled raw table. `since` is clamped to the
// caller's plan retention before the query runs.
func (s *Store) QueryCronResults(customerID uint, watchName string, since time.Time) ([]CronResult, error) {
	var watch CronWatch
	if err := s.db.Where("customer_id = ? AND name = ?", customerID, watchName).First(&watch).Error; err != nil {
		return nil, err
	}
	since = clampRetention(s.TierForCustomer(customerID), since)
	var results []CronResult
	err := s.db.Where("cron_watch_id = ? AND ran_at >= ?", watch.ID, since).
		Order("ran_at DESC").Find(&results).Error
	return results, err
}

// pickGranularity returns the coarsest rollup tier whose retention
// covers the requested window. Boundaries match the pg_partman
// retention ceilings in migration 026.
func pickGranularity(window time.Duration) string {
	switch {
	case window <= 7*24*time.Hour:
		return "raw"
	case window <= 14*24*time.Hour:
		return "hourly"
	case window <= 35*24*time.Hour:
		return "4h"
	default:
		return "daily"
	}
}

// clampRetention pushes `since` forward to (now - LogRetention(tier))
// when the requested window exceeds the plan's retention, so callers
// never read rows older than the plan allows — even though the rows
// may still exist on disk.
func clampRetention(tier Tier, since time.Time) time.Time {
	cap := time.Now().Add(-LogRetention(tier))
	if since.Before(cap) {
		return cap
	}
	return since
}

// TierForCustomer returns the active subscription tier for a customer.
// Empty Tier ("" → trial retention in LogRetention) when no active
// sub is found — conservatively treat unknown customers as trial so
// we never over-serve data.
//
// NOTE: subscriptions.customer_id is a TEXT column that stores the
// customer *name*, not the numeric id (see store/subscription.go).
// That's historical — we'd rename it if we were starting over.
func (s *Store) TierForCustomer(customerID uint) Tier {
	var tier string
	err := s.db.Raw(`
		SELECT s.tier
		FROM subscriptions s
		JOIN customers c ON s.customer_id = c.name
		WHERE c.id = ?
		  AND s.status = 'active'
		  AND (s.expires_at IS NULL OR s.expires_at > now())
		ORDER BY s.created_at DESC
		LIMIT 1`, customerID).Scan(&tier).Error
	if err != nil {
		return ""
	}
	return Tier(tier)
}

// LatestCronResult returns the most recent result for a named watch.
func (s *Store) LatestCronResult(customerID uint, watchName string) (*CronResult, error) {
	var watch CronWatch
	if err := s.db.Where("customer_id = ? AND name = ?", customerID, watchName).First(&watch).Error; err != nil {
		return nil, err
	}
	var result CronResult
	err := s.db.Where("cron_watch_id = ?", watch.ID).Order("ran_at DESC").First(&result).Error
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// --- Notification Config ---

// NotifyConfig stores per-customer notification channel state.
// Slack credentials live here (OAuth bot token; webhook kept only for
// tenants that never re-authed). PagerDuty integration keys live in
// their own pagerduty_integrations table keyed by (customer_id, name).
// AliasesYAML is the raw /etc/notify.conf document — a map of alias →
// per-service entries consumed by the Notifier.
type NotifyConfig struct {
	gorm.Model
	CustomerID    uint   `gorm:"not null;uniqueIndex"`
	SlackWebhook  string // incoming webhook URL (optional)
	SlackBotToken string // bot OAuth token for chat.postMessage
	SlackTeamName string // workspace name (for display)
	AliasesYAML   string `gorm:"type:text"`
	ConfiguredAt  *time.Time
}

// PagerDutyIntegration is a single named PagerDuty integration key for a
// customer. notify.conf aliases reference rows here by Name.
type PagerDutyIntegration struct {
	gorm.Model
	CustomerID     uint   `gorm:"not null;index:idx_pd_integrations_customer_name,unique,where:deleted_at IS NULL"`
	Name           string `gorm:"not null;index:idx_pd_integrations_customer_name,unique,where:deleted_at IS NULL"`
	IntegrationKey string `gorm:"not null"`
	LastUsedAt     *time.Time
	LastError      string
}

// TableName pins the plural table name even though the model name doesn't
// pluralize naturally through gorm's NamingStrategy.
func (PagerDutyIntegration) TableName() string { return "pagerduty_integrations" }

func (s *Store) GetNotifyConfig(customerID uint) (*NotifyConfig, error) {
	var nc NotifyConfig
	err := s.db.Where("customer_id = ?", customerID).First(&nc).Error
	if err != nil {
		return nil, err
	}
	return &nc, nil
}

func (s *Store) SaveSlackWebhook(customerID uint, webhook, teamName string) error {
	return s.SaveSlackConfig(customerID, webhook, "", teamName)
}

func (s *Store) SaveSlackConfig(customerID uint, webhook, botToken, teamName string) error {
	now := time.Now()
	var nc NotifyConfig
	result := s.db.Where("customer_id = ?", customerID).First(&nc)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Create(&NotifyConfig{
			CustomerID:    customerID,
			SlackWebhook:  webhook,
			SlackBotToken: botToken,
			SlackTeamName: teamName,
			ConfiguredAt:  &now,
		}).Error
	}
	return s.db.Model(&nc).Updates(map[string]any{
		"slack_webhook":   webhook,
		"slack_bot_token": botToken,
		"slack_team_name": teamName,
		"configured_at":   &now,
	}).Error
}

func (s *Store) DisconnectSlack(customerID uint) error {
	return s.db.Model(&NotifyConfig{}).Where("customer_id = ?", customerID).Updates(map[string]any{
		"slack_webhook":   "",
		"slack_team_name": "",
	}).Error
}

// SaveNotifyAliases writes the raw /etc/notify.conf YAML for a customer.
// Callers must validate the YAML before invoking this.
func (s *Store) SaveNotifyAliases(customerID uint, aliasesYAML string) error {
	var nc NotifyConfig
	result := s.db.Where("customer_id = ?", customerID).First(&nc)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Create(&NotifyConfig{
			CustomerID:  customerID,
			AliasesYAML: aliasesYAML,
		}).Error
	}
	return s.db.Model(&nc).Update("aliases_yaml", aliasesYAML).Error
}

// ── PagerDuty integrations ─────────────────────────────

// ListPagerDutyIntegrations returns all PD integrations for a customer.
func (s *Store) ListPagerDutyIntegrations(customerID uint) ([]PagerDutyIntegration, error) {
	var items []PagerDutyIntegration
	err := s.db.Where("customer_id = ?", customerID).Order("name").Find(&items).Error
	return items, err
}

// GetPagerDutyIntegration returns a single integration by name.
func (s *Store) GetPagerDutyIntegration(customerID uint, name string) (*PagerDutyIntegration, error) {
	var item PagerDutyIntegration
	err := s.db.Where("customer_id = ? AND name = ?", customerID, name).First(&item).Error
	if err != nil {
		return nil, err
	}
	return &item, nil
}

// SavePagerDutyIntegration inserts or updates a PD integration by (customer, name).
func (s *Store) SavePagerDutyIntegration(customerID uint, name, key string) error {
	var item PagerDutyIntegration
	result := s.db.Where("customer_id = ? AND name = ?", customerID, name).First(&item)
	if result.Error == gorm.ErrRecordNotFound {
		return s.db.Create(&PagerDutyIntegration{
			CustomerID:     customerID,
			Name:           name,
			IntegrationKey: key,
		}).Error
	}
	return s.db.Model(&item).Update("integration_key", key).Error
}

// DeletePagerDutyIntegration removes a single integration by name.
func (s *Store) DeletePagerDutyIntegration(customerID uint, name string) error {
	return s.db.Where("customer_id = ? AND name = ?", customerID, name).Delete(&PagerDutyIntegration{}).Error
}

// DeleteAllPagerDutyIntegrations clears all integrations for a customer.
func (s *Store) DeleteAllPagerDutyIntegrations(customerID uint) error {
	return s.db.Where("customer_id = ?", customerID).Delete(&PagerDutyIntegration{}).Error
}

// RecordPagerDutySend marks an integration's last_used_at / last_error after
// a dispatch attempt. sendErr may be empty on success.
func (s *Store) RecordPagerDutySend(customerID uint, name, sendErr string) error {
	now := time.Now()
	return s.db.Model(&PagerDutyIntegration{}).
		Where("customer_id = ? AND name = ?", customerID, name).
		Updates(map[string]any{
			"last_used_at": &now,
			"last_error":   sendErr,
		}).Error
}
