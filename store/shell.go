package store

import (
	"context"
	"crypto/sha256"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/alert"
	"github.com/graybeardcorp/weft/planner"
	luaparse "github.com/yuin/gopher-lua/parse"
	"gorm.io/gorm"
)

// ParseScheduleInterval returns the fixed cadence at which a stored schedule
// fires. Schedules that pass ValidateSchedule map exactly; anything else is a
// row saved before validation existed, and keeps the lenient reading it has
// always had so that it keeps firing as it did (see legacyScheduleInterval).
func ParseScheduleInterval(schedule string) time.Duration {
	if d, err := ValidateSchedule(schedule); err == nil {
		return d
	}
	return legacyScheduleInterval(schedule)
}

const (
	scheduleDay  = 24 * time.Hour
	scheduleWeek = 7 * scheduleDay
)

// ValidateSchedule parses a schedule the strict way, for save paths. Crons
// fire on a UTC-aligned grid of uniform intervals, never at a chosen time of
// day, so a schedule that names one (or that cannot be read at all) is an
// error rather than a cron quietly running at some other cadence.
//
// Accepted forms:
//
//   - "every <duration>", with w, d, h, m and s units that may be combined
//     ("every 90s", "every 1d", "every 1w2d")
//   - "* * * * *"      every minute
//   - "*/N * * * *"    every N minutes
//   - "0 * * * *"      every hour
//   - "0 */H * * *"    every H hours
//   - "0 0 * * *"      every day, closing 00:00 UTC
//   - "0 0 */N * *"    every N days
//   - "0 0 * * 1"      every week, closing Monday 00:00 UTC
func ValidateSchedule(schedule string) (time.Duration, error) {
	unrecognized := fmt.Errorf("unrecognized schedule %q: use a crontab such as */5 * * * *, 0 * * * *, 0 0 * * * or 0 0 * * 1, or an interval such as every 5m, every 1d or every 1w", schedule)

	if rest, ok := strings.CutPrefix(schedule, "every "); ok {
		d, ok := parseIntervalDuration(strings.TrimSpace(rest))
		if !ok {
			return 0, unrecognized
		}
		if d <= 0 {
			return 0, fmt.Errorf("schedule %q: interval must be positive", schedule)
		}
		return d, nil
	}

	f := strings.Fields(schedule)
	if len(f) != 5 || f[3] != "*" {
		return 0, unrecognized
	}
	minute, hour, dom, dow := f[0], f[1], f[2], f[4]

	if minute == "*" || strings.HasPrefix(minute, "*/") {
		if hour != "*" || dom != "*" || dow != "*" {
			return 0, unrecognized
		}
		if minute == "*" {
			return time.Minute, nil
		}
		n, ok := cronStep(minute)
		if !ok {
			return 0, unrecognized
		}
		return time.Duration(n) * time.Minute, nil
	}

	if !isFixedField(minute, 59) {
		return 0, unrecognized
	}
	if minute != "0" {
		return 0, fmt.Errorf("schedule %q: crons fire on the hour, so the minute field must be 0 (or use */N for every N minutes)", schedule)
	}

	if hour == "*" || strings.HasPrefix(hour, "*/") {
		if dom != "*" || dow != "*" {
			return 0, unrecognized
		}
		if hour == "*" {
			return time.Hour, nil
		}
		n, ok := cronStep(hour)
		if !ok {
			return 0, unrecognized
		}
		return time.Duration(n) * time.Hour, nil
	}

	if !isFixedField(hour, 23) {
		return 0, unrecognized
	}
	if hour != "0" {
		return 0, fmt.Errorf("schedule %q: daily and weekly crons close at 00:00 UTC and cannot run at a chosen time of day; use 0 0 * * * (or every 1d)", schedule)
	}

	switch {
	case dom == "*" && dow == "*":
		return scheduleDay, nil
	case dow == "*":
		n, ok := cronStep(dom)
		if !ok {
			return 0, unrecognized
		}
		return time.Duration(n) * scheduleDay, nil
	case dom == "*" && dow == "1":
		return scheduleWeek, nil
	case dom == "*" && isFixedField(dow, 7):
		return 0, fmt.Errorf("schedule %q: weekly crons close on Monday 00:00 UTC; use 0 0 * * 1 (or every 1w)", schedule)
	}
	return 0, unrecognized
}

// parseIntervalDuration reads a combined duration of w, d, h, m and s units
// ("1w2d", "1h30m"). It is time.ParseDuration plus days and weeks, without
// fractions.
func parseIntervalDuration(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var total time.Duration
	for s != "" {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i == 0 || i == len(s) {
			return 0, false
		}
		n, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		var unit time.Duration
		switch s[i] {
		case 'w':
			unit = scheduleWeek
		case 'd':
			unit = scheduleDay
		case 'h':
			unit = time.Hour
		case 'm':
			unit = time.Minute
		case 's':
			unit = time.Second
		default:
			return 0, false
		}
		total += time.Duration(n) * unit
		s = s[i+1:]
	}
	return total, true
}

// legacyScheduleInterval is the lenient reader schedules were parsed with
// before ValidateSchedule: only the minute and hour fields count, a fixed
// minute is ignored, and anything unrecognized is 5 minutes. It remains only
// so rows saved under those rules keep their cadence.
func legacyScheduleInterval(schedule string) time.Duration {
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
	refs, err := scriptRefs(language, source)
	if err != nil {
		return err
	}
	if err := s.refuseTextRefs(orgID, refs, ""); err != nil {
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
		if err := validateWeftFunctionsAreNotPiped(script); err != nil {
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
		return fmt.Errorf("alert may only appear as the final stage of a script or cron body, not in a leading list, loop body, conditional, or nested command: it fires once per run, wherever it sits (to page several places, point --notify at one notify.yaml alias with several targets)")
	}
	return nil
}

// stdinWeftCommands are the weft:: commands that read standard input; every
// other weft:: function takes its cron name and window as arguments and
// ignores stdin.
var stdinWeftCommands = map[string]bool{"weft::is_gt": true, "weft::is_lt": true}

// validateWeftFunctionsAreNotPiped rejects a weft:: function that is not the
// first stage of its pipeline. Those functions read stored cron history, never
// their input, so whatever is piped in is silently thrown away, which is never
// what the author meant. The stdin comparators are the way to test a piped
// value.
func validateWeftFunctionsAreNotPiped(script *bash.Script) error {
	if script == nil {
		return nil
	}
	var offender string
	var stage int
	bash.Walk(script, func(n bash.Node) bool {
		if offender != "" {
			return false
		}
		p, ok := n.(*bash.Pipeline)
		if !ok {
			return true
		}
		for i, c := range p.Commands {
			sc, ok := c.(*bash.SimpleCommand)
			if !ok || i == 0 || len(sc.Words) == 0 {
				continue
			}
			name := sc.Words[0].Literal()
			if strings.HasPrefix(name, "weft::") && !stdinWeftCommands[name] {
				offender, stage = name, i+1
				return false
			}
		}
		return true
	})
	if offender != "" {
		return fmt.Errorf("%s is stage %d of a pipeline, but it never reads its input, so what is piped into it is discarded; to test a piped value, use weft::is_gt or weft::is_lt", offender, stage)
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

// CronWatch is a scheduled pipeline run. ValueField and TagField are the
// cron's output schema, from the alert stage's -v/-t or from the
// `# value:`/`# tag:` headers; nil means the default text rule
// (`<value> [<tag>]` per line). They're populated by SaveCronWatch so the
// executor doesn't reparse the pipeline every tick. Stored as text —
// either a 1-indexed column number ("3") or a dot-prefixed JSON path
// (".foo.bar"); see shellcore.FieldSpec.
type CronWatch struct {
	gorm.Model
	OrgID OrgID  `gorm:"not null"`
	Name  string `gorm:"not null"`
	// NamespaceID is the cron's home namespace; nil is an org-level cron.
	// A name is unique within its home (migration 112).
	NamespaceID *uint  `gorm:"column:namespace_id"`
	Schedule    string `gorm:"not null"` // "*/5 * * * *" or "every 1m"
	Pipeline    string `gorm:"type:text;not null"`
	// Enabled has no gorm default: with one, gorm swaps an explicit false
	// for true on create. The column's own default (schema.sql) covers raw SQL.
	Enabled bool `gorm:"not null"`
	// ConsecutiveStrikes counts runs in a row that ran out of time (the run
	// budget, a delivery timeout); at the bench threshold the cron is
	// disabled. A clean run,
	// and any save that re-enables the cron, resets it.
	ConsecutiveStrikes int `gorm:"column:consecutive_strikes;not null;default:0"`
	LastRunAt          *time.Time
	LastStatus         string  // "ok", "error", "alert", "timeout", "invalid"
	ValueField         *string `gorm:"column:value_field"`
	TagField           *string `gorm:"column:tag_field"`
	// HasAlert is true when the pipeline has an alert stage. A cron that
	// alerts without -v stores its output as one untagged value.
	HasAlert bool `gorm:"column:has_alert;not null;default:false"`
	// ValueType is ValueNumber, or ValueText for a `# type: text` cron,
	// whose values are stored as text and refused by numeric readers.
	ValueType string `gorm:"column:value_type;not null;default:number"`
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
	LoweredPlan JSONB `gorm:"column:lowered_plan;type:jsonb"`
}

// JSONB is JSON bytes bound for a jsonb column. Queries run in pgx's exec mode
// (see Connect), which sends parameters as text, so a plain []byte would go as
// bytea hex that jsonb can't parse; JSONB goes as the JSON text itself.
type JSONB []byte

func (j JSONB) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	return string(j), nil
}

func (j *JSONB) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append(JSONB(nil), v...)
	case string:
		*j = JSONB(v)
	default:
		return fmt.Errorf("jsonb: cannot scan %T", src)
	}
	return nil
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
func (s *Store) SaveCronWatch(orgID OrgID, ns uint, userID, name, schedule, pipeline string, enabled bool, loweredPlan []byte) error {
	if _, err := ValidateSchedule(schedule); err != nil {
		return err
	}
	compiled, err := CompileWatchPipeline(pipeline)
	if err != nil {
		return err
	}
	if err := s.checkValueTypes(orgID, ns, name, compiled); err != nil {
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
		result := inCronHome(tx.Unscoped(), ns).Where("name = ?", name).First(&w)
		if result.Error == gorm.ErrRecordNotFound {
			row := &CronWatch{
				OrgID:          orgID,
				Name:           name,
				NamespaceID:    cronHome(ns),
				Schedule:       schedule,
				Pipeline:       pipeline,
				Enabled:        enabled,
				ValueField:     compiled.ValueField,
				TagField:       compiled.TagField,
				HasAlert:       compiled.HasAlert,
				ValueType:      compiled.ValueType,
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
		if w.ContentHash == hash && !w.DeletedAt.Valid && w.Enabled == enabled {
			return nil
		}
		// Existing watches keep their original creator on overwrite.
		updates := map[string]any{
			"deleted_at":      nil,
			"schedule":        schedule,
			"pipeline":        pipeline,
			"content_hash":    hash,
			"value_field":     compiled.ValueField,
			"tag_field":       compiled.TagField,
			"has_alert":       compiled.HasAlert,
			"value_type":      compiled.ValueType,
			"alert_condition": compiled.AlertCondition,
			"alert_threshold": compiled.AlertThreshold,
			"lowered_plan":    JSONB(loweredPlan),
			"enabled":         enabled,
		}
		if enabled && !w.Enabled {
			// Turning a benched cron back on wipes its strikes.
			updates["consecutive_strikes"] = 0
		}
		return tx.Unscoped().Model(&w).Updates(updates).Error
	}); err != nil {
		return err
	}

	return lintSavedWatch(schedule, pipeline, loweredPlan)
}

// ColdGrazeWindow is how long a cron's window must be before a fire can reach
// archived data: the 14-day recent-data window less a day for the lag between
// a shard's window ending and its data moving (docs/plans/cold-scan-cap.md).
// A fixed plan fact, like the hot window in scripts/costmodel/tiers.yaml.
const ColdGrazeWindow = 13 * 24 * time.Hour

// coldGrazeWarning is the save-time notice for a cron whose window can reach
// older data, or "" when it cannot. The window of each fire is the schedule's
// interval.
func coldGrazeWarning(schedule string) string {
	interval, err := ValidateSchedule(schedule)
	if err != nil || interval < ColdGrazeWindow {
		return ""
	}
	return fmt.Sprintf("This cron looks back %s at each fire, close to the 14 days of recent data, so a fire can reach older data. "+
		"Older data counts against your monthly cold history budget. Once that is used up, a fire that needs it fails, the cron is disabled, "+
		"and a notice goes to your `system` alias, so define one to be told. Shorten the schedule to stay within recent data.",
		formatWindow(interval))
}

// formatWindow renders a look-back in days, or weeks when it is whole weeks.
func formatWindow(d time.Duration) string {
	days := int(d / (24 * time.Hour))
	if days%7 == 0 {
		return fmt.Sprintf("%d weeks", days/7)
	}
	return fmt.Sprintf("%d days", days)
}

// lintSavedWatch runs every save-time, non-blocking check against a
// watch that just saved successfully, and joins their messages into
// one Warning-only error (nil if there's nothing to say). loweredPlan
// may be nil (Lower() failed at save time, e.g. an unsupported
// pipeline shape) — the checks that depend on it just no-op then;
// this never blocks a save that already succeeded.
func lintSavedWatch(schedule, pipeline string, loweredPlan []byte) error {
	// The schedule check needs no plan, so it runs even when Lower() failed.
	var msgs []string
	if m := coldGrazeWarning(schedule); m != "" {
		msgs = append(msgs, m)
	}
	var lp planner.LoweredPlan
	if len(loweredPlan) > 0 && json.Unmarshal(loweredPlan, &lp) == nil {
		msgs = append(planner.LintTruncationRisk(&lp), msgs...)
		// A cron pays for a stage that runs post-retrieval on every fire, so
		// say so once, at save. A pipeline with nothing to gain stays silent,
		// as does one that calls a /bin script: its stages aren't analyzed.
		if len(lp.BinDeps) == 0 {
			if a, err := planner.Analyze(pipeline, "/", nil); err == nil && a.Barrier != 0 {
				msgs = append(msgs, "This cron runs on every fire, so a stage that runs post-retrieval is paid for each time.\n\n"+strings.TrimRight(a.Render(false), "\n"))
			}
		}
	}
	if len(msgs) == 0 {
		return nil
	}
	warnings := make([]error, len(msgs))
	for i, m := range msgs {
		warnings[i] = NewWarning("%s", m)
	}
	return errors.Join(warnings...)
}

// CompiledWatch holds the pre-parsed bits of a cron that we want to stamp
// onto the watch row so the executor doesn't re-parse on every tick.
// ValueField/TagField/HasAlert are runtime-relevant (they shape how
// cron_results gets written); AlertCondition/Threshold are dashboard
// projections for threshold-line rendering and are only populated for
// conditions with a single numeric cutoff (above, below, eq, neq).
type CompiledWatch struct {
	// ValueField/TagField are the output schema, stamped as the FieldSpec
	// source form ("3" or ".foo.bar"); the executor re-parses them via
	// shellcore.ParseFieldSpec to recover the FieldSpec.
	ValueField     *string
	TagField       *string
	HasAlert       bool
	ValueType      string // ValueNumber or ValueText
	AlertCondition *string
	AlertThreshold *float64
	// NumericRefs are the crons this pipeline reads with a numeric weft::
	// function, by literal name; a name built at run time isn't listed.
	NumericRefs []string
}

// Cron value types: ValueNumber is the default, ValueText is `# type: text`.
const (
	ValueNumber = "number"
	ValueText   = "text"
)

// valueTypeHeader reads a cron's `# type:` header; text is the only type
// it names, and a cron without one stores numbers.
func valueTypeHeader(pipeline string) (string, error) {
	typ := ValueNumber
	seen := false
	for _, line := range strings.Split(pipeline, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "# type:") {
			continue
		}
		if seen {
			return "", fmt.Errorf("'# type:' is given twice")
		}
		seen = true
		if v := strings.TrimSpace(strings.TrimPrefix(trimmed, "# type:")); v != ValueText {
			return "", fmt.Errorf("'# type:' takes text, got %q; a cron without it stores numbers", v)
		}
		typ = ValueText
	}
	return typ, nil
}

// schemaHeaders reads a cron's `# value:` and `# tag:` header comments.
// They name JSON paths only: text output has a fixed positional rule.
func schemaHeaders(pipeline string) (value, tag shellcore.FieldSpec, err error) {
	seen := map[string]bool{}
	for _, line := range strings.Split(pipeline, "\n") {
		trimmed := strings.TrimSpace(line)
		var key string
		switch {
		case strings.HasPrefix(trimmed, "# value:"):
			key = "value"
		case strings.HasPrefix(trimmed, "# tag:"):
			key = "tag"
		default:
			continue
		}
		if seen[key] {
			return value, tag, fmt.Errorf("'# %s:' is given twice", key)
		}
		seen[key] = true
		src := strings.TrimSpace(strings.TrimPrefix(trimmed, "# "+key+":"))
		spec, perr := shellcore.ParseFieldSpec(src)
		if perr != nil || !spec.IsJSON() {
			return value, tag, fmt.Errorf("'# %s:' takes a JSON path like .host, got %q; text output is <value> [<tag>] per line and needs no header", key, src)
		}
		if key == "value" {
			value = spec
		} else {
			tag = spec
		}
	}
	if !tag.IsZero() && value.IsZero() {
		return value, tag, fmt.Errorf("'# tag:' needs a '# value:' header")
	}
	return value, tag, nil
}

// CompileWatchPipeline validates pipeline-shape invariants and returns the
// cron's output schema and the compiled bits of its (at most one) `alert`
// stage.
//
// Validation rules:
//   - The pipeline must parse as bash.
//   - `alert` may appear at most once in the pipeline. The single stage is
//     the sole incident-dedup boundary per watch run.
//   - `-v` / `-t` must be a positive integer or a `.`-prefixed JSON path
//     if present (enforced by alert.ParseArgs).
//   - `# value:` / `# tag:` headers name JSON paths, and a cron with an
//     alert stage declares its schema with -v/-t instead.
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
	if err := validateAlertPosition(script); err != nil {
		return out, err
	}
	if err := validateWeftFunctionsAreNotPiped(script); err != nil {
		return out, err
	}
	headerValue, headerTag, err := schemaHeaders(pipeline)
	if err != nil {
		return out, err
	}
	if out.ValueType, err = valueTypeHeader(pipeline); err != nil {
		return out, err
	}
	if out.NumericRefs, err = bashRefs(script); err != nil {
		return out, err
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
	if len(alertCmds) == 0 {
		if !headerValue.IsZero() {
			s := headerValue.String()
			out.ValueField = &s
		}
		if !headerTag.IsZero() {
			s := headerTag.String()
			out.TagField = &s
		}
		return out, nil
	}
	out.HasAlert = true
	argv := make([]string, 0, len(alertCmds[0].Words)-1)
	for _, w := range alertCmds[0].Words[1:] {
		argv = append(argv, w.Literal())
	}
	parsed, perr := alert.ParseArgs(argv)
	if perr != nil {
		return out, fmt.Errorf("alert: %w", perr)
	}
	if !headerValue.IsZero() {
		if !parsed.Key.IsZero() || !parsed.Tag.IsZero() {
			return out, fmt.Errorf("this cron sets its schema twice (# value: and alert -v); keep one")
		}
		return out, fmt.Errorf("a cron that alerts declares its schema with alert -v %s, not '# value:'", headerValue)
	}
	if !parsed.Key.IsZero() {
		s := parsed.Key.String()
		out.ValueField = &s
	}
	if !parsed.Tag.IsZero() {
		s := parsed.Tag.String()
		out.TagField = &s
	}
	switch parsed.Condition {
	case alert.Any, alert.Absent, alert.Changed:
	case alert.Matches:
		if out.ValueType != ValueText {
			return out, fmt.Errorf("alert matches reads text; add '# type: text' to store this cron's values as text")
		}
	default:
		if out.ValueType == ValueText {
			return out, fmt.Errorf("this cron stores text (# type: text), and alert %s compares numbers; use any, absent, changed or matches", parsed.Condition)
		}
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
	return out, nil
}

// ValidateWatchPipeline is the error-only form of CompileWatchPipeline for
// callers that don't need the compiled extractors.
func ValidateWatchPipeline(pipeline string) error {
	_, err := CompileWatchPipeline(pipeline)
	return err
}

// Home is the cron's home namespace id, 0 at the org level.
func (w *CronWatch) Home() uint {
	if w.NamespaceID == nil {
		return 0
	}
	return *w.NamespaceID
}

// cronHome is the namespace_id for a home: 0 is the org level.
func cronHome(ns uint) *uint {
	if ns == 0 {
		return nil
	}
	return &ns
}

// inCronHome limits a cron query to one home namespace, or to the org level
// when ns is 0.
func inCronHome(q *gorm.DB, ns uint) *gorm.DB {
	if ns == 0 {
		return q.Where("namespace_id IS NULL")
	}
	return q.Where("namespace_id = ?", ns)
}

// GetCronWatch finds a cron by its home (ns, 0 for the org level) and name.
func (s *Store) GetCronWatch(orgID OrgID, ns uint, name string) (*CronWatch, error) {
	var w CronWatch
	err := inCronHome(s.scoped(orgID), ns).Where("name = ?", name).First(&w).Error
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

func (s *Store) DeleteCronWatch(orgID OrgID, ns uint, name string) error {
	return inCronHome(s.scoped(orgID), ns).Where("name = ?", name).Delete(&CronWatch{}).Error
}

// CountCronWatches returns the number of enabled cron watches for a customer:
// a disabled cron is free, so it does not count against the plan limit.
func (s *Store) CountCronWatches(orgID OrgID) (int, error) {
	var count int64
	err := s.scoped(orgID).Model(&CronWatch{}).Where("enabled = ?", true).Count(&count).Error
	return int(count), err
}

// RecordCronStrike adds a strike to a cron whose run ran out of time and
// disables it once it has strikes in a row, reporting whether this call
// benched it. One UPDATE, so concurrent coordinators can't both miss the
// threshold.
func (s *Store) RecordCronStrike(ctx context.Context, id uint, strikes int) (benched bool, err error) {
	var enabled bool
	res := s.DB().WithContext(WithoutTenantScope(ctx)).Raw(
		`UPDATE cron_watches
		    SET consecutive_strikes = consecutive_strikes + 1,
		        enabled = enabled AND consecutive_strikes + 1 < ?
		  WHERE id = ?
		  RETURNING enabled`, strikes, id).Scan(&enabled)
	if res.Error != nil {
		return false, res.Error
	}
	if res.RowsAffected == 0 {
		return false, fmt.Errorf("cron %d: no such watch", id)
	}
	return !enabled, nil
}

// DisableCron turns a cron off at once, for a run that failed in a way
// every run would, reporting whether this call disabled it (false if it
// was already off), so concurrent coordinators page about it once.
func (s *Store) DisableCron(ctx context.Context, id uint) (disabled bool, err error) {
	res := s.DB().WithContext(WithoutTenantScope(ctx)).Exec(
		`UPDATE cron_watches SET enabled = false WHERE id = ? AND enabled`, id)
	return res.RowsAffected == 1, res.Error
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
	// TextValue is the value of a `# type: text` cron's row; Value is
	// NULL for those, so they never reach the rollups.
	TextValue *string `gorm:"column:text_value"`
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

// How long each result tier is kept. These mirror the pg_partman retention
// set on the cron_results tables in schema.sql; keep them in step.
const (
	RawRetention      = 7 * 24 * time.Hour
	HourlyRetention   = 14 * 24 * time.Hour
	FourHourRetention = 35 * 24 * time.Hour
	DailyRetention    = 400 * 24 * time.Hour
)

// PickGranularity returns the coarsest rollup tier whose retention
// covers the requested window, for callers that read one tier (charts).
// Statistics over a window use CronWindowStats, which stitches tiers.
func PickGranularity(window time.Duration) string {
	switch {
	case window <= RawRetention:
		return "raw"
	case window <= HourlyRetention:
		return "hourly"
	case window <= FourHourRetention:
		return "4h"
	default:
		return "daily"
	}
}

// CronWindowStats is the exact sum-level statistics of a cron's numeric
// results over a window. Count is 0 when the window held none.
type CronWindowStats struct {
	Count int64
	Sum   float64
	SumSq float64
	Min   float64
	Max   float64
}

// Mean is Sum/Count.
func (c CronWindowStats) Mean() float64 { return c.Sum / float64(c.Count) }

// Stddev is the population standard deviation, derived from the sums so it
// stays exact however the buckets were merged.
func (c CronWindowStats) Stddev() float64 {
	mean := c.Mean()
	v := c.SumSq/float64(c.Count) - mean*mean
	if v < 0 {
		v = 0 // float cancellation on near-constant series
	}
	return math.Sqrt(v)
}

// resultSegment is one tier's slice of a window. A zero `to` is open-ended.
type resultSegment struct {
	granularity string
	from, to    time.Time
}

// windowSegments splits the window [since, now] across the result tiers,
// finest tier for the most recent data: raw for the last RawRetention, then
// hourly, 4-hour and daily buckets further back. The segments are disjoint
// and meet at bucket boundaries, so every result is counted once. Only the
// oldest edge is approximate: it is rounded up to a bucket boundary of the
// tier it falls in, so the window can start up to one bucket (at most a day)
// later than asked.
func windowSegments(now, since time.Time) []resultSegment {
	ceil := func(t time.Time, d time.Duration) time.Time {
		if r := t.Truncate(d); r.Equal(t) {
			return t
		} else {
			return r.Add(d)
		}
	}
	const day = 24 * time.Hour
	rawFrom := ceil(now.Add(-RawRetention), time.Hour)
	hourlyFrom := ceil(now.Add(-HourlyRetention), 4*time.Hour)
	fourHourFrom := ceil(now.Add(-FourHourRetention), day)

	var out []resultSegment
	add := func(gran string, floor, to time.Time, align time.Duration) {
		from := floor
		if lo := ceil(since, align); lo.After(from) {
			from = lo
		}
		if !to.IsZero() && !from.Before(to) {
			return
		}
		out = append(out, resultSegment{gran, from, to})
	}
	// Raw is exact: no alignment, and `since` bounds it directly.
	if since.Before(now.Add(-RawRetention)) {
		out = append(out, resultSegment{"raw", rawFrom, time.Time{}})
	} else {
		out = append(out, resultSegment{"raw", since, time.Time{}})
	}
	add("hourly", hourlyFrom, rawFrom, time.Hour)
	add("4h", fourHourFrom, hourlyFrom, 4*time.Hour)
	add("daily", time.Time{}, fourHourFrom, day)
	// Oldest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// CronWindowStats aggregates every numeric result of a cron since `since`
// (callers apply their plan's ClampRetention first), reading raw results for
// recent data and the rollups for older data. All tags are combined.
func (s *Store) CronWindowStats(cronWatchID uint, since time.Time) (CronWindowStats, error) {
	var total CronWindowStats
	for _, seg := range windowSegments(time.Now(), since) {
		q := `SELECT COALESCE(SUM(count), 0) AS count, COALESCE(SUM(sum), 0) AS sum,
			COALESCE(SUM(sum_sq), 0) AS sum_sq, COALESCE(MIN(min), 0) AS min, COALESCE(MAX(max), 0) AS max
			FROM cron_results_all
			WHERE granularity = ? AND cron_watch_id = ? AND bucket_start >= ?`
		args := []any{seg.granularity, cronWatchID, seg.from}
		if !seg.to.IsZero() {
			q += " AND bucket_start < ?"
			args = append(args, seg.to)
		}
		var part CronWindowStats
		if err := s.db.Raw(q, args...).Scan(&part).Error; err != nil {
			return CronWindowStats{}, err
		}
		if part.Count == 0 {
			continue
		}
		if total.Count == 0 {
			total = part
			continue
		}
		total.Count += part.Count
		total.Sum += part.Sum
		total.SumSq += part.SumSq
		total.Min = math.Min(total.Min, part.Min)
		total.Max = math.Max(total.Max, part.Max)
	}
	return total, nil
}

// CronSeries is one cron's stored points over a window, read from a single
// tier: the coarsest whose retention still covers the window's start.
type CronSeries struct {
	Granularity string
	Start, End  time.Time // the window after clamping to the plan's retention
	Buckets     []CronBucket
}

// CronSeries returns a cron's buckets with start <= bucket_start < end,
// oldest first, every tag included. start is clamped to the org's
// retention first.
func (s *Store) CronSeries(orgID OrgID, cronWatchID uint, start, end time.Time) (CronSeries, error) {
	start = ClampRetention(s.TierForOrg(orgID), start)
	out := CronSeries{Granularity: PickGranularity(time.Since(start)), Start: start, End: end}
	if !start.Before(end) {
		return out, nil
	}
	err := s.db.Raw(`
		SELECT bucket_start, tag, count, sum, sum_sq, min, max
		FROM cron_results_all
		WHERE granularity = ?
		  AND cron_watch_id = ?
		  AND bucket_start >= ?
		  AND bucket_start < ?
		  AND count > 0
		ORDER BY bucket_start ASC, tag ASC`,
		out.Granularity, cronWatchID, start, end,
	).Scan(&out.Buckets).Error
	return out, err
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

// SaveNotifyAliases writes the raw /etc/notify.yaml YAML for a customer. The
// YAML must pass ValidateNotifyAliases, so it can never be saved without the
// system alias.
func (s *Store) SaveNotifyAliases(orgID OrgID, userID, aliasesYAML string) error {
	if err := ValidateNotifyAliases(aliasesYAML); err != nil {
		return err
	}
	aliases, _ := ParseNotifyAliases(aliasesYAML) // parsed once already by the validation above
	if err := s.checkSecretRefs(orgID, aliases); err != nil {
		return err
	}
	return s.scoped(orgID).Transaction(func(tx *gorm.DB) error {
		// The history trigger records the acting user as the version's author.
		if err := tx.Exec("SELECT set_config('weft.acting_user_id', ?, true)", userID).Error; err != nil {
			return err
		}
		var nc NotifyConfig
		result := tx.First(&nc)
		if result.Error == gorm.ErrRecordNotFound {
			return tx.Create(&NotifyConfig{
				OrgID:       orgID,
				AliasesYAML: aliasesYAML,
			}).Error
		}
		return tx.Model(&nc).Update("aliases_yaml", aliasesYAML).Error
	})
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
