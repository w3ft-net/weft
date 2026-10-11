package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/weft/alert"
	"github.com/graybeardcorp/weft/planner"
	luaparse "github.com/yuin/gopher-lua/parse"
	"gorm.io/gorm"
)

// ParseScheduleInterval parses a schedule string into the fixed cadence
// at which the cron fires. Every schedule in this system expresses a
// uniform interval, never a wall-clock-specific firing time, so only the
// minute and hour fields matter (day-of-month/month/day-of-week are
// always "*"):
//
//   - "every <duration>" (e.g. "every 90s")
//   - minute field "*"          -> 1 minute
//   - minute field "*/N"        -> N minutes
//   - minute field a fixed value, paired with:
//   - hour field "*"          -> 1 hour
//   - hour field "*/H"        -> H hours
//
// Returns 5m for unrecognized formats. Previously only "*/N * * * *" was
// handled and everything else — including the common "* * * * *" and
// "0 * * * *" shapes — silently fell through to that 5m default, which
// meant crons saved with a 1-minute schedule (a subscription-tier
// feature; see MinWatchInterval) or an hourly schedule actually fired
// every 5 minutes instead. See TestParseScheduleInterval_BareStar.
func ParseScheduleInterval(schedule string) time.Duration {
	if strings.HasPrefix(schedule, "every ") {
		if d, err := time.ParseDuration(strings.TrimPrefix(schedule, "every ")); err == nil {
			return d
		}
	}

	fields := strings.Fields(schedule)
	if len(fields) == 5 {
		minute, hour := fields[0], fields[1]
		if minute == "*" {
			return time.Minute
		}
		if n, ok := cronStep(minute); ok {
			return time.Duration(n) * time.Minute
		}
		if isFixedField(minute, 59) {
			if hour == "*" {
				return time.Hour
			}
			if n, ok := cronStep(hour); ok {
				return time.Duration(n) * time.Hour
			}
		}
	}

	return 5 * time.Minute
}

// cronStep parses a "*/N" cron field, returning (N, true) if it's a
// valid positive step, or (0, false) otherwise.
func cronStep(field string) (int, bool) {
	nStr, ok := strings.CutPrefix(field, "*/")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(nStr)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// isFixedField reports whether field is a literal integer in [0, max] —
// i.e. a specific minute/hour rather than "*" or a "*/N" step.
func isFixedField(field string, max int) bool {
	n, err := strconv.Atoi(field)
	return err == nil && n >= 0 && n <= max
}

// ShellFunction is a user-defined function stored in the virtual filesystem.
// Language is detected from shebang at write time: "bash" (default) or "lua".
type ShellFunction struct {
	gorm.Model
	OrgID           OrgID   `gorm:"not null;uniqueIndex:idx_func_customer_name"`
	Name            string  `gorm:"not null;uniqueIndex:idx_func_customer_name"`
	Source          string  `gorm:"type:text;not null"`
	Language        string  `gorm:"not null;default:bash"` // "bash" or "lua"
	ContentHash     string  `gorm:"column:content_hash"`
	Message         string  `gorm:"column:message"`
	CreatedByUserID *string `gorm:"column:created_by_user_id"`
}

// SaveShellFunction persists a /bin/<name> entry. The caller is
// responsible for classifying the script — language must be "bash"
// or "lua", and source must already have any shebang stripped if
// language == "lua" (the lua parser rejects shebang-prefixed
// sources). The bin-mount-domain classifier lives in
// mounts.ClassifyBinScript; the store stays oblivious to the
// .lua-extension convention so name only matters here as the row
// key, never as a language signal.
func (s *Store) SaveShellFunction(orgID OrgID, userID, name, source, language string) error {
	if err := validateSource(language, source); err != nil {
		return err
	}
	hash := computeShellFunctionHash(source)

	return s.scoped(orgID).Transaction(func(tx *gorm.DB) error {
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

		// Unscoped: a soft-deleted row still owns the (org, name) unique
		// slot, so saving over a deleted name revives it in place and its
		// history continues.
		var fn ShellFunction
		result := tx.Unscoped().Where("name = ?", name).First(&fn)
		if result.Error == gorm.ErrRecordNotFound {
			row := &ShellFunction{
				OrgID: orgID, Name: name,
				Source: source, Language: language,
				ContentHash: hash,
			}
			if userID != "" {
				row.CreatedByUserID = &userID
			}
			return tx.Create(row).Error
		}
		// No-op save (e.g. :w on an unchanged buffer): skip the UPDATE
		// so HEAD's updated_at doesn't drift and history isn't
		// polluted. The DB-level trigger guard would also prevent the
		// history INSERT, but skipping here saves the roundtrip.
		if fn.ContentHash == hash && !fn.DeletedAt.Valid {
			return nil
		}
		// Existing rows keep their original creator on overwrite —
		// only source / language / hash change. Editing isn't authoring.
		// A deleted row is revived (deleted_at cleared) by the same write.
		return tx.Unscoped().Model(&fn).Updates(map[string]any{
			"source":       source,
			"language":     language,
			"content_hash": hash,
			"deleted_at":   nil,
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

func (s *Store) GetShellFunction(orgID OrgID, name string) (*ShellFunction, error) {
	var fn ShellFunction
	err := s.scoped(orgID).Where("name = ?", name).First(&fn).Error
	if err != nil {
		return nil, err
	}
	return &fn, nil
}

func (s *Store) ListShellFunctions(orgID OrgID) ([]ShellFunction, error) {
	var fns []ShellFunction
	err := s.scoped(orgID).Order("name").Find(&fns).Error
	return fns, err
}

func (s *Store) DeleteShellFunction(orgID OrgID, name string) error {
	return s.scoped(orgID).Where("name = ?", name).Delete(&ShellFunction{}).Error
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
	OrgID      OrgID  `gorm:"not null;uniqueIndex:idx_cron_customer_name"`
	Name       string `gorm:"not null;uniqueIndex:idx_cron_customer_name"`
	Schedule   string `gorm:"not null"` // "*/5 * * * *" or "every 1m"
	Pipeline   string `gorm:"type:text;not null"`
	Enabled    bool   `gorm:"not null;default:true"`
	LastRunAt  *time.Time
	LastStatus string  // "ok", "error", "stderr"
	AlertKey   *string `gorm:"column:alert_key"`
	AlertTag   *string `gorm:"column:alert_tag"`
	// AlertCondition + AlertThreshold are the dashboard projection of
	// the `alert` stage's numeric cutoff. Populated only for
	// above/below/eq/neq — conditions without a single absolute
	// threshold (rising/falling/changed/absent/any) leave both NULL.
	AlertCondition  *string  `gorm:"column:alert_condition"`
	AlertThreshold  *float64 `gorm:"column:alert_threshold"`
	ContentHash     string   `gorm:"column:content_hash"`
	Message         string   `gorm:"column:message"`
	CreatedByUserID *string  `gorm:"column:created_by_user_id"`
	// LoweredPlan is the JSON-encoded planner.LoweredPlan produced at
	// save time. NULL when the pipeline couldn't be lowered (e.g. a
	// /bin/ reference the lowering pass can't inline yet); the fire
	// path falls back to inline planner.Plan() in that case. JSONB
	// in Postgres so operators can introspect via `SELECT
	// lowered_plan -> 'shard_sql' FROM cron_watches`.
	LoweredPlan []byte `gorm:"column:lowered_plan;type:jsonb"`
}

// computeCronWatchHash returns the canonical content hash for a
// cron watch — SHA-256 over schedule + "\n" + pipeline. Matches
// the Postgres backfill expression in migration 036.
func computeCronWatchHash(schedule, pipeline string) string {
	h := sha256.Sum256([]byte(schedule + "\n" + pipeline))
	return hex.EncodeToString(h[:])
}

// SaveCronWatch persists a watch row. loweredPlan is the JSON-encoded
// planner.LoweredPlan from a save-time Lower() call; pass nil when
// the pipeline didn't lower (e.g. /bin/ inlining isn't implemented
// for this shape yet) — the fire path falls back to inline Plan().
//
// The returned error may be a non-blocking store.Warning (or an
// errors.Join of several) — the save already happened; callers use
// store.Split to separate an actual rejection from a heads-up to
// show the user. Today the only producer is the truncation lint
// below, but this is a generic channel: any future save-time check
// that wants to warn without blocking uses the same mechanism.
func (s *Store) SaveCronWatch(orgID OrgID, userID, name, schedule, pipeline string, loweredPlan []byte) error {
	compiled, err := CompileWatchPipeline(pipeline)
	if err != nil {
		return err
	}
	hash := computeCronWatchHash(schedule, pipeline)

	if err := s.scoped(orgID).Transaction(func(tx *gorm.DB) error {
		// See SaveShellFunction for why we always set, even for empty.
		if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", userID).Error; err != nil {
			return err
		}
		// Unscoped: see SaveShellFunction — saving a deleted name revives it.
		var w CronWatch
		result := tx.Unscoped().Where("name = ?", name).First(&w)
		if result.Error == gorm.ErrRecordNotFound {
			row := &CronWatch{
				OrgID:          orgID,
				Name:           name,
				Schedule:       schedule,
				Pipeline:       pipeline,
				Enabled:        true,
				AlertKey:       compiled.AlertKey,
				AlertTag:       compiled.AlertTag,
				AlertCondition: compiled.AlertCondition,
				AlertThreshold: compiled.AlertThreshold,
				ContentHash:    hash,
				LoweredPlan:    loweredPlan,
			}
			if userID != "" {
				row.CreatedByUserID = &userID
			}
			return tx.Create(row).Error
		}
		// No-op save: skip the UPDATE. See SaveShellFunction for why.
		if w.ContentHash == hash && !w.DeletedAt.Valid {
			return nil
		}
		// Existing watches keep their original creator on overwrite.
		updates := map[string]any{
			"deleted_at":      nil,
			"schedule":        schedule,
			"pipeline":        pipeline,
			"content_hash":    hash,
			"alert_key":       compiled.AlertKey,
			"alert_tag":       compiled.AlertTag,
			"alert_condition": compiled.AlertCondition,
			"alert_threshold": compiled.AlertThreshold,
			"lowered_plan":    loweredPlan,
		}
		if w.DeletedAt.Valid {
			updates["enabled"] = true // a revived watch starts enabled, like a new one
		}
		return tx.Unscoped().Model(&w).Updates(updates).Error
	}); err != nil {
		return err
	}

	return lintSavedWatch(loweredPlan)
}

// lintSavedWatch runs every save-time, non-blocking check against a
// watch that just saved successfully, and joins their messages into
// one Warning-only error (nil if there's nothing to say). loweredPlan
// may be nil (Lower() failed at save time, e.g. an unsupported
// pipeline shape) — the checks that depend on it just no-op then;
// this never blocks a save that already succeeded.
func lintSavedWatch(loweredPlan []byte) error {
	if len(loweredPlan) == 0 {
		return nil
	}
	var lp planner.LoweredPlan
	if err := json.Unmarshal(loweredPlan, &lp); err != nil {
		return nil
	}
	msgs := planner.LintTruncationRisk(&lp)
	if len(msgs) == 0 {
		return nil
	}
	warnings := make([]error, len(msgs))
	for i, m := range msgs {
		warnings[i] = NewWarning("%s", m)
	}
	return errors.Join(warnings...)
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
//     if present (enforced by alert.ParseArgs).
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
		parsed, perr := alert.ParseArgs(argv)
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
		case alert.Above, alert.Below, alert.Eq, alert.Neq:
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

func (s *Store) GetCronWatch(orgID OrgID, name string) (*CronWatch, error) {
	var w CronWatch
	err := s.scoped(orgID).Where("name = ?", name).First(&w).Error
	if err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) ListCronWatches(orgID OrgID) ([]CronWatch, error) {
	var watches []CronWatch
	err := s.scoped(orgID).Order("name").Find(&watches).Error
	return watches, err
}

func (s *Store) DeleteCronWatch(orgID OrgID, name string) error {
	return s.scoped(orgID).Where("name = ?", name).Delete(&CronWatch{}).Error
}

// CountCronWatches returns the number of cron watches for a customer.
func (s *Store) CountCronWatches(orgID OrgID) (int, error) {
	var count int64
	err := s.scoped(orgID).Model(&CronWatch{}).Count(&count).Error
	return int(count), err
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

// QueryCronResults returns results for a named watch within a time range,
// scoped to a customer. Used by weft.* functions in /bin/ scripts.
//
// Raw-only — the full CronResult row (with stdout/stderr/exit_code)
// only exists in the unrolled raw table. `since` is clamped to the
// caller's plan retention before the query runs.
func (s *Store) QueryCronResults(orgID OrgID, watchName string, since time.Time) ([]CronResult, error) {
	var watch CronWatch
	if err := s.scoped(orgID).Where("name = ?", watchName).First(&watch).Error; err != nil {
		return nil, err
	}
	since = ClampRetention(s.TierForOrg(orgID), since)
	var results []CronResult
	err := s.db.Where("cron_watch_id = ? AND ran_at >= ?", watch.ID, since).
		Order("ran_at DESC").Find(&results).Error
	return results, err
}

// PickGranularity returns the coarsest rollup tier whose retention
// covers the requested window. Boundaries match the pg_partman
// retention ceilings in migration 026.
func PickGranularity(window time.Duration) string {
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

// ClampRetention pushes `since` forward to (now - LogRetention(tier))
// when the requested window exceeds the plan's retention, so callers
// never read rows older than the plan allows — even though the rows
// may still exist on disk.
func ClampRetention(tier Tier, since time.Time) time.Time {
	cap := time.Now().Add(-LogRetention(tier))
	if since.Before(cap) {
		return cap
	}
	return since
}

// TierForOrg returns the active subscription tier for a customer.
// Empty Tier ("" → trial retention in LogRetention) when no active
// sub is found — conservatively treat unknown customers as trial so
// we never over-serve data.
//
// NOTE: subscriptions.org_id is a TEXT column that stores the
// customer *name*, not the numeric id (see store/subscription.go).
// That's historical — we'd rename it if we were starting over.
func (s *Store) TierForOrg(orgID OrgID) Tier {
	var tier string
	err := s.db.Raw(`
		SELECT s.tier
		FROM subscriptions s
		WHERE s.org_id = ?
		  AND s.status = 'active'
		  AND (s.expires_at IS NULL OR s.expires_at > now())
		ORDER BY s.created_at DESC
		LIMIT 1`, orgID).Scan(&tier).Error
	if err != nil {
		return ""
	}
	return Tier(tier)
}

// --- Notification Config ---

// NotifyConfig stores per-customer notification channel state.
// Slack credentials live here (OAuth bot token; webhook kept only for
// tenants that never re-authed). PagerDuty integration keys live in
// their own pagerduty_integrations table keyed by (org_id, name).
// AliasesYAML is the raw /etc/notify.yaml document — a map of alias →
// per-service entries consumed by the Notifier.
type NotifyConfig struct {
	gorm.Model
	OrgID         OrgID  `gorm:"not null;uniqueIndex"`
	SlackWebhook  string // incoming webhook URL (optional)
	SlackBotToken string // bot OAuth token for chat.postMessage
	SlackTeamName string // workspace name (for display)
	AliasesYAML   string `gorm:"type:text"`
	ConfiguredAt  *time.Time
}

// PagerDutyIntegration is a single named PagerDuty integration key for a
// customer. notify.yaml aliases reference rows here by Name.
type PagerDutyIntegration struct {
	gorm.Model
	OrgID          OrgID  `gorm:"not null;index:idx_pd_integrations_customer_name,unique,where:deleted_at IS NULL"`
	Name           string `gorm:"not null;index:idx_pd_integrations_customer_name,unique,where:deleted_at IS NULL"`
	IntegrationKey string `gorm:"not null"`
	LastUsedAt     *time.Time
	LastError      string
}

// TableName pins the plural table name even though the model name doesn't
// pluralize naturally through gorm's NamingStrategy.
func (PagerDutyIntegration) TableName() string { return "pagerduty_integrations" }

func (s *Store) GetNotifyConfig(orgID OrgID) (*NotifyConfig, error) {
	var nc NotifyConfig
	err := s.scoped(orgID).First(&nc).Error
	if err != nil {
		return nil, err
	}
	return &nc, nil
}

func (s *Store) SaveSlackConfig(orgID OrgID, webhook, botToken, teamName string) error {
	now := time.Now()
	var nc NotifyConfig
	result := s.scoped(orgID).First(&nc)
	if result.Error == gorm.ErrRecordNotFound {
		return s.scoped(orgID).Create(&NotifyConfig{
			OrgID:         orgID,
			SlackWebhook:  webhook,
			SlackBotToken: botToken,
			SlackTeamName: teamName,
			ConfiguredAt:  &now,
		}).Error
	}
	return s.scoped(orgID).Model(&nc).Updates(map[string]any{
		"slack_webhook":   webhook,
		"slack_bot_token": botToken,
		"slack_team_name": teamName,
		"configured_at":   &now,
	}).Error
}

// SaveNotifyAliases writes the raw /etc/notify.yaml YAML for a customer.
// Callers must validate the YAML before invoking this.
func (s *Store) SaveNotifyAliases(orgID OrgID, aliasesYAML string) error {
	var nc NotifyConfig
	result := s.scoped(orgID).First(&nc)
	if result.Error == gorm.ErrRecordNotFound {
		return s.scoped(orgID).Create(&NotifyConfig{
			OrgID:       orgID,
			AliasesYAML: aliasesYAML,
		}).Error
	}
	return s.scoped(orgID).Model(&nc).Update("aliases_yaml", aliasesYAML).Error
}

// ── PagerDuty integrations ─────────────────────────────

// RecordPagerDutySend marks an integration's last_used_at / last_error after
// a dispatch attempt. sendErr may be empty on success.
func (s *Store) RecordPagerDutySend(orgID OrgID, name, sendErr string) error {
	now := time.Now()
	return s.scoped(orgID).Model(&PagerDutyIntegration{}).
		Where("name = ?", name).
		Updates(map[string]any{
			"last_used_at": &now,
			"last_error":   sendErr,
		}).Error
}
