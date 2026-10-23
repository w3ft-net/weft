package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	luapkg "github.com/graybeardcorp/warp/pkg/lua"
	"github.com/graybeardcorp/weft/store"
	lua "github.com/yuin/gopher-lua"
	"gorm.io/gorm"
)

// maxAggDuration is the longest window for the aggregates that can be built
// from sums (avg, min, max, sum, stddev, count, rate): they read the rollup
// tiers as well as raw results, so they reach as far back as the daily tier
// is kept. Your plan's retention can shorten that further.
const maxAggDuration = store.DailyRetention

// maxRawDuration is the longest window for funcs that need individual
// results (percentile, first, delta, slope, is_seasonal, is_absent). Only
// raw results hold those; rollups keep sums, not values.
const maxRawDuration = store.RawRetention

// dayWeekUnit matches a number followed by the d (day) or w (week) unit.
var dayWeekUnit = regexp.MustCompile(`(\d+(?:\.\d+)?)([dw])`)

// parseDuration is time.ParseDuration plus the day (d, 24h) and week (w,
// 7d) units, which the docs and `range` use and Go's parser rejects. Units
// combine as in Go ("1d12h", "1w2d"). A day is a fixed 24 hours: windows
// are elapsed time, not calendar time, so DST doesn't stretch them.
func parseDuration(s string) (time.Duration, error) {
	expanded := dayWeekUnit.ReplaceAllStringFunc(s, func(m string) string {
		n, _ := strconv.ParseFloat(m[:len(m)-1], 64)
		hours := n * 24
		if m[len(m)-1] == 'w' {
			hours = n * 24 * 7
		}
		return strconv.FormatFloat(hours, 'f', -1, 64) + "h"
	})
	d, err := time.ParseDuration(expanded)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

// WeftFuncs provides cron result query functions for /bin/ scripts.
// Both bash (ExternalCmdFunc) and Lua (weft.* table) use this.
//
// Ctx is the executing request's context. The warp function interface
// these methods satisfy carries no ctx per call, so it is bound once per
// execution (runLocal) and used for the DB lookups; nil (tests) means
// context.Background().
type WeftFuncs struct {
	Store *store.Store
	OrgID store.OrgID
	Ctx   context.Context
}

func (w *WeftFuncs) ctx() context.Context {
	if w.Ctx == nil {
		return context.Background()
	}
	return w.Ctx
}

// SeasonalStatsResult is the {avg, stddev, samples} triple over the
// subset of a watch's history restricted to "same time of period."
type SeasonalStatsResult struct {
	Avg     float64
	Stddev  float64
	Samples int
}

func (w *WeftFuncs) Latest(watchName string) (string, error) {
	r, err := func(orgID store.OrgID, watchName string) (*store.CronResult, error) {
		var watch store.CronWatch
		if err := w.Store.DB().WithContext(store.WithOrg(w.ctx(), orgID)).Where("name = ?", watchName).First(&watch).Error; err != nil {
			return nil, err
		}
		var result store.CronResult
		err := w.Store.DB().Where("cron_watch_id = ?", watch.ID).Order("ran_at DESC").First(&result).Error
		if err != nil {
			return nil, err
		}
		return &result, nil
	}(w.OrgID, watchName)
	if err != nil {
		return "", fmt.Errorf("weft::latest: cron %q: %w", watchName, err)
	}
	return strings.TrimSpace(r.Stdout), nil
}

// Absent reports whether watchName has no result: within the last window
// when window > 0, otherwise at all (no result, or an empty latest one). A
// cron that doesn't exist and one that has never run are both absent, so a
// typo'd or deleted canary fires its dead-man's switch loudly instead of
// staying silent. An error means history couldn't be read (or the window is
// invalid or over the cap): that is "can't tell", never "absent", so a bad
// argument or a database hiccup can't page anyone.
func (w *WeftFuncs) Absent(watchName string, window time.Duration) (bool, error) {
	if window > 0 {
		id, since, err := w.rawWindow("weft::is_absent", watchName, window, false)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		// Every result counts here, numeric or not: a text-only canary that
		// ran is not absent.
		var n int64
		if err := w.Store.DB().Raw(`SELECT COUNT(*) FROM cron_results WHERE cron_watch_id = ? AND ran_at >= ?`, id, since).Scan(&n).Error; err != nil {
			return false, fmt.Errorf("weft::is_absent: cron %q: %w", watchName, err)
		}
		return n == 0, nil
	}
	s, err := w.Latest(watchName)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return s == "", nil
}

// maxDescribeRuns caps how many runs weft::describe returns, matching the
// most the cron detail page ever loads.
const maxDescribeRuns = 50

// defaultDescribeRuns is how many runs weft::describe returns when n is omitted.
const defaultDescribeRuns = 10

// RunRecord is one saved cron result as weft::describe reports it: every
// field the run recorded, including the ones no other weft:: function
// exposes (exit code, duration, stderr).
type RunRecord struct {
	RanAt      time.Time `json:"ran_at"`
	ExitCode   int       `json:"exit_code"`
	DurationMs int64     `json:"duration_ms"`
	Tag        string    `json:"tag"`
	Value      *float64  `json:"value"`
	Text       *string   `json:"text,omitempty"` // a `# type: text` cron's value
	Stdout     string    `json:"stdout"`
	Stderr     string    `json:"stderr"`
}

// Describe returns the n most recent saved results of a cron, newest first.
// A multi-row cron saves one result per line, so n counts results, not
// firings.
func (w *WeftFuncs) Describe(watchName string, n int) ([]RunRecord, error) {
	if n < 1 || n > maxDescribeRuns {
		return nil, fmt.Errorf("weft::describe: n must be between 1 and %d", maxDescribeRuns)
	}
	var watch store.CronWatch
	if err := w.Store.DB().WithContext(store.WithOrg(w.ctx(), w.OrgID)).Where("name = ?", watchName).First(&watch).Error; err != nil {
		return nil, fmt.Errorf("weft::describe: cron %q: %w", watchName, err)
	}
	results, err := w.Store.ListCronResults(watch.ID, n, "")
	if err != nil {
		return nil, fmt.Errorf("weft::describe: cron %q: %w", watchName, err)
	}
	out := make([]RunRecord, len(results))
	for i, r := range results {
		out[i] = RunRecord{
			RanAt: r.RanAt.UTC(), ExitCode: r.ExitCode, DurationMs: r.DurationMs,
			Tag: r.Tag, Value: r.Value, Text: r.TextValue, Stdout: r.Stdout, Stderr: r.Stderr,
		}
	}
	return out, nil
}

// writeRunsJSONL writes one compact JSON object per run.
func writeRunsJSONL(out io.Writer, runs []RunRecord) error {
	enc := json.NewEncoder(out)
	for _, r := range runs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

// Previous returns the trimmed stdout of the second-most-recent
// result for a named watch — the value just before Latest. Used by
// changed/rising/falling predicates for prev-tick semantics.
// Returns "" with no error when fewer than 2 results exist.
func (w *WeftFuncs) Previous(watchName string) (string, error) {
	cw, err := w.Store.GetCronWatch(w.OrgID, watchName)
	if err != nil {
		return "", fmt.Errorf("weft::previous: cron %q: %w", watchName, err)
	}
	results, err := w.Store.ListCronResults(cw.ID, 2, "")
	if err != nil {
		return "", fmt.Errorf("weft::previous: cron %q: %w", watchName, err)
	}
	if len(results) < 2 {
		return "", nil
	}
	return strings.TrimSpace(results[1].Stdout), nil
}

// windowStats aggregates a cron's numeric results over the last dur, reading
// raw results for recent data and the rollups for older data. It is the
// shared core of the sum-based funcs.
func (w *WeftFuncs) windowStats(fn, watchName string, dur time.Duration) (store.CronWindowStats, error) {
	if dur <= 0 {
		return store.CronWindowStats{}, fmt.Errorf("duration must be positive")
	}
	if dur > maxAggDuration {
		return store.CronWindowStats{}, fmt.Errorf("duration %s exceeds maximum of %s", dur, maxAggDuration)
	}
	var watch store.CronWatch
	if err := w.Store.DB().WithContext(store.WithOrg(w.ctx(), w.OrgID)).Where("name = ?", watchName).First(&watch).Error; err != nil {
		return store.CronWindowStats{}, fmt.Errorf("%s: cron %q: %w", fn, watchName, err)
	}
	if err := w.numericCron(fn, watch); err != nil {
		return store.CronWindowStats{}, err
	}
	since := store.ClampRetention(w.Store.TierForOrg(w.OrgID), time.Now().Add(-dur))
	st, err := w.Store.CronWindowStats(watch.ID, since)
	if err != nil {
		return store.CronWindowStats{}, fmt.Errorf("%s: cron %q: %w", fn, watchName, err)
	}
	return st, nil
}

// nonEmptyStats is windowStats for funcs that have no answer without data.
func (w *WeftFuncs) nonEmptyStats(fn, watchName string, dur time.Duration) (store.CronWindowStats, error) {
	st, err := w.windowStats(fn, watchName, dur)
	if err != nil {
		return st, err
	}
	if st.Count == 0 {
		return st, fmt.Errorf("%s: no numeric results for %q", fn, watchName)
	}
	return st, nil
}

func (w *WeftFuncs) Avg(watchName string, dur time.Duration) (float64, error) {
	st, err := w.nonEmptyStats("weft::avg", watchName, dur)
	return st.Mean(), err
}

func (w *WeftFuncs) Max(watchName string, dur time.Duration) (float64, error) {
	st, err := w.nonEmptyStats("weft::max", watchName, dur)
	return st.Max, err
}

func (w *WeftFuncs) Min(watchName string, dur time.Duration) (float64, error) {
	st, err := w.nonEmptyStats("weft::min", watchName, dur)
	return st.Min, err
}

func (w *WeftFuncs) Sum(watchName string, dur time.Duration) (float64, error) {
	st, err := w.nonEmptyStats("weft::sum", watchName, dur)
	return st.Sum, err
}

// Stddev is the population standard deviation, computed from the sums.
func (w *WeftFuncs) Stddev(watchName string, dur time.Duration) (float64, error) {
	st, err := w.windowStats("weft::stddev", watchName, dur)
	if err != nil {
		return 0, err
	}
	if st.Count < 2 {
		return 0, fmt.Errorf("weft::stddev: need at least 2 results for %q", watchName)
	}
	return st.Stddev(), nil
}

// Count returns the number of numeric results recorded within the window.
// Results whose output wasn't a number are not counted, because the rollups
// that serve long windows don't keep them.
func (w *WeftFuncs) Count(watchName string, dur time.Duration) (float64, error) {
	st, err := w.windowStats("weft::count", watchName, dur)
	return float64(st.Count), err
}

// errTextCron marks a numeric read of a `# type: text` cron: a mistake in
// the script, not missing data, so callers that answer "can't tell" for
// missing data report it instead.
var errTextCron = errors.New("stores text")

// numericCron refuses a text cron for a function that needs numbers. Saves
// refuse literal names already; this covers names built at run time. In a
// cron run the refusal disables the cron (noteTypeError): it would fail
// the same way every run.
func (w *WeftFuncs) numericCron(fn string, watch store.CronWatch) error {
	if watch.ValueType != store.ValueText {
		return nil
	}
	err := fmt.Errorf("%s: cron %q %w (# type: text), so it has no numbers; weft::latest reads it", fn, watch.Name, errTextCron)
	noteTypeError(w.ctx(), err)
	return err
}

// CheckNumeric refuses a text cron for fn. A cron that doesn't exist, or
// can't be read, passes: fn itself reports those.
func (w *WeftFuncs) CheckNumeric(fn, watchName string) error {
	var watch store.CronWatch
	if err := w.Store.DB().WithContext(store.WithOrg(w.ctx(), w.OrgID)).Where("name = ?", watchName).First(&watch).Error; err != nil {
		return nil
	}
	return w.numericCron(fn, watch)
}

// rawWindow finds the cron and the start of a raw-results window, enforcing
// the raw cap shared by the funcs that need individual values; numeric
// refuses a text cron.
func (w *WeftFuncs) rawWindow(fn, watchName string, dur time.Duration, numeric bool) (watchID uint, since time.Time, err error) {
	if dur <= 0 {
		return 0, time.Time{}, fmt.Errorf("duration must be positive")
	}
	if dur > maxRawDuration {
		return 0, time.Time{}, fmt.Errorf("duration %s exceeds maximum of %s", dur, maxRawDuration)
	}
	var watch store.CronWatch
	if err := w.Store.DB().WithContext(store.WithOrg(w.ctx(), w.OrgID)).Where("name = ?", watchName).First(&watch).Error; err != nil {
		return 0, time.Time{}, fmt.Errorf("%s: cron %q: %w", fn, watchName, err)
	}
	if numeric {
		if err := w.numericCron(fn, watch); err != nil {
			return 0, time.Time{}, err
		}
	}
	return watch.ID, store.ClampRetention(w.Store.TierForOrg(w.OrgID), time.Now().Add(-dur)), nil
}

// First returns the chronologically oldest numeric result in the window, the
// anchor for delta and slope.
func (w *WeftFuncs) First(watchName string, dur time.Duration) (float64, error) {
	first, _, n, err := w.rawEnds("weft::first", watchName, dur)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("weft::first: no numeric results for %q", watchName)
	}
	return first, nil
}

// rawEnds returns the oldest and newest numeric result in the window and how
// many there are.
func (w *WeftFuncs) rawEnds(fn, watchName string, dur time.Duration) (first, last float64, n int64, err error) {
	id, since, err := w.rawWindow(fn, watchName, dur, true)
	if err != nil {
		return 0, 0, 0, err
	}
	var row struct {
		First float64
		Last  float64
		N     int64
	}
	err = w.Store.DB().Raw(`
		SELECT COUNT(*) AS n,
		       COALESCE((ARRAY_AGG(value ORDER BY ran_at ASC))[1], 0)  AS first,
		       COALESCE((ARRAY_AGG(value ORDER BY ran_at DESC))[1], 0) AS last
		FROM cron_results
		WHERE cron_watch_id = ? AND ran_at >= ? AND value IS NOT NULL`, id, since).Scan(&row).Error
	if err != nil {
		return 0, 0, 0, fmt.Errorf("%s: cron %q: %w", fn, watchName, err)
	}
	return row.First, row.Last, row.N, nil
}

// Percentile returns the p-th percentile (p in 0..100) of numeric results in
// the window using linear interpolation between adjacent ranks (R-7 / Excel /
// numpy default; Postgres percentile_cont). p=0 is min, p=50 is median,
// p=100 is max. Percentiles need individual values, so the window is limited
// to raw results.
func (w *WeftFuncs) Percentile(watchName string, dur time.Duration, p float64) (float64, error) {
	if p < 0 || p > 100 {
		return 0, fmt.Errorf("weft::percentile: p must be in [0, 100], got %v", p)
	}
	id, since, err := w.rawWindow("weft::percentile", watchName, dur, true)
	if err != nil {
		return 0, err
	}
	var row struct {
		Pct *float64
	}
	if err := w.Store.DB().Raw(`
		SELECT percentile_cont(?::float8) WITHIN GROUP (ORDER BY value) AS pct
		FROM cron_results
		WHERE cron_watch_id = ? AND ran_at >= ? AND value IS NOT NULL`, p/100.0, id, since).Scan(&row).Error; err != nil {
		return 0, fmt.Errorf("weft::percentile: cron %q: %w", watchName, err)
	}
	if row.Pct == nil {
		return 0, fmt.Errorf("weft::percentile: no numeric results for %q", watchName)
	}
	return *row.Pct, nil
}

// Delta returns newest numeric result minus oldest numeric result in the
// window. Composite of weft::latest and weft::first.
func (w *WeftFuncs) Delta(watchName string, dur time.Duration) (float64, error) {
	first, last, n, err := w.rawEnds("weft::delta", watchName, dur)
	if err != nil {
		return 0, err
	}
	if n < 2 {
		return 0, fmt.Errorf("weft::delta: need at least 2 numeric results for %q", watchName)
	}
	return last - first, nil
}

// Slope returns Delta / dur.Seconds(), the average rate of change per second
// across the window. Caller multiplies for other units.
func (w *WeftFuncs) Slope(watchName string, dur time.Duration) (float64, error) {
	d, err := w.Delta(watchName, dur)
	if err != nil {
		return 0, err
	}
	return d / dur.Seconds(), nil
}

// Rate returns Sum / dur.Seconds(): events per second when the cron's output
// is a per-period count. Caller multiplies for per-minute / per-hour.
func (w *WeftFuncs) Rate(watchName string, dur time.Duration) (float64, error) {
	s, err := w.Sum(watchName, dur)
	if err != nil {
		return 0, err
	}
	return s / dur.Seconds(), nil
}

// SeasonalStats returns avg/stddev/samples of the watch's history
// restricted to "same time of period" — e.g. for period=1d and
// window=4d it computes over the readings from this hour on each of
// the last four days. Used by the seasonal predicate and (future)
// alert seasonal condition. Cap is maxRawDuration since it needs
// individual results. The result type lives
// in warp's lua package to keep the WeftFuncs interface standalone.
func (w *WeftFuncs) SeasonalStats(watchName string, period, window time.Duration) (SeasonalStatsResult, error) {
	if window > maxRawDuration {
		return SeasonalStatsResult{}, fmt.Errorf("weft::is_seasonal: window %s exceeds maximum of %s", window, maxRawDuration)
	}
	if window <= 0 || period <= 0 {
		return SeasonalStatsResult{}, fmt.Errorf("weft::is_seasonal: window and period must be positive")
	}
	if period >= window {
		return SeasonalStatsResult{}, fmt.Errorf("weft::is_seasonal: window must be larger than period")
	}
	avg, stddev, samples, err := func(orgID store.OrgID, watchName string, period, window time.Duration) (avg, stddev float64, samples int, err error) {
		var watch store.CronWatch
		if err := w.Store.DB().WithContext(store.WithOrg(w.ctx(), orgID)).Where("name = ?", watchName).First(&watch).Error; err != nil {
			return 0, 0, 0, err
		}
		if err := w.numericCron("weft::is_seasonal", watch); err != nil {
			return 0, 0, 0, err
		}
		now := time.Now()
		since := store.ClampRetention(w.Store.TierForOrg(orgID), now.Add(-window))
		tol := period / 20
		if tol > 5*time.Minute {
			tol = 5 * time.Minute
		}
		periodSec := period.Seconds()
		tolSec := tol.Seconds()
		var row struct {
			Avg     *float64
			Samples int
		}
		avgQ := `
			SELECT AVG(value) AS avg, COUNT(*) AS samples
			FROM cron_results
			WHERE cron_watch_id = ?
			  AND ran_at >= ?
			  AND value IS NOT NULL
			  AND LEAST(
			        MOD(EXTRACT(EPOCH FROM (? - ran_at))::numeric, ?::numeric),
			        ?::numeric - MOD(EXTRACT(EPOCH FROM (? - ran_at))::numeric, ?::numeric)
			      ) <= ?`
		if err := w.Store.DB().Raw(avgQ, watch.ID, since, now, periodSec, periodSec, now, periodSec, tolSec).Scan(&row).Error; err != nil {
			return 0, 0, 0, err
		}
		samples = row.Samples
		if samples == 0 || row.Avg == nil {
			return 0, 0, samples, nil
		}
		avg = *row.Avg
		var sd struct{ Stddev *float64 }
		sdQ := `
			SELECT STDDEV_SAMP(value) AS stddev
			FROM cron_results
			WHERE cron_watch_id = ?
			  AND ran_at >= ?
			  AND value IS NOT NULL
			  AND LEAST(
			        MOD(EXTRACT(EPOCH FROM (? - ran_at))::numeric, ?::numeric),
			        ?::numeric - MOD(EXTRACT(EPOCH FROM (? - ran_at))::numeric, ?::numeric)
			      ) <= ?`
		if err := w.Store.DB().Raw(sdQ, watch.ID, since, now, periodSec, periodSec, now, periodSec, tolSec).Scan(&sd).Error; err != nil {
			return avg, 0, samples, err
		}
		if sd.Stddev != nil {
			stddev = *sd.Stddev
		}
		return avg, stddev, samples, nil
	}(w.OrgID, watchName, period, window)
	if err != nil {
		return SeasonalStatsResult{}, fmt.Errorf("weft::is_seasonal: cron %q: %w", watchName, err)
	}
	return SeasonalStatsResult{Avg: avg, Stddev: stddev, Samples: samples}, nil
}

// ParseAggDuration parses a duration string (30m, 1h, 6h, 1d, 30d) for the
// sum-based aggregates, allowing windows up to maxAggDuration.
func ParseAggDuration(s string) (time.Duration, error) {
	d, err := parseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g., 30m, 1h, 6h, 1d, 30d)", s)
	}
	if d > maxAggDuration {
		return 0, fmt.Errorf("duration %s exceeds maximum of %s", d, maxAggDuration)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return d, nil
}

// ParseRawDuration parses a duration string for the funcs that need
// individual results (percentile, first, delta, slope), allowing windows up
// to maxRawDuration.
func ParseRawDuration(s string) (time.Duration, error) {
	d, err := parseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g., 1h, 24h, 7d)", s)
	}
	if d > maxRawDuration {
		return 0, fmt.Errorf("duration %s exceeds maximum of %s", d, maxRawDuration)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return d, nil
}

// weftQueryFuncs is the cron-result query surface the weft.* Lua
// table is built on top of. *WeftFuncs is the production
// implementation; tests substitute a stub via this interface.
type weftQueryFuncs interface {
	Latest(name string) (string, error)
	Previous(name string) (string, error)
	Describe(name string, n int) ([]RunRecord, error)
	CheckNumeric(fn, name string) error
	Avg(name string, dur time.Duration) (float64, error)
	Max(name string, dur time.Duration) (float64, error)
	Min(name string, dur time.Duration) (float64, error)
	Sum(name string, dur time.Duration) (float64, error)
	Stddev(name string, dur time.Duration) (float64, error)
	First(name string, dur time.Duration) (float64, error)
	Count(name string, dur time.Duration) (float64, error)
	Absent(name string, window time.Duration) (bool, error)
	Percentile(name string, dur time.Duration, p float64) (float64, error)
	Delta(name string, dur time.Duration) (float64, error)
	Slope(name string, dur time.Duration) (float64, error)
	Rate(name string, dur time.Duration) (float64, error)
	SeasonalStats(name string, period, window time.Duration) (SeasonalStatsResult, error)
}

// LuaExtension returns a luapkg.Extension that installs the `weft.*`
// table on a Lua sandbox: value primitives (latest/previous/avg/...)
// plus boolean predicates (is_above/is_below/is_rising/...). Wire it
// onto an interp via `interp.LuaExtensions = []luapkg.Extension{wf.LuaExtension()}`.
func (w *WeftFuncs) LuaExtension() luapkg.Extension {
	return buildWeftLuaExtension(w)
}

func buildWeftLuaExtension(w weftQueryFuncs) luapkg.Extension {
	return func(L *lua.LState) {
		mod := L.NewTable()

		mod.RawSetString("latest", L.NewFunction(func(L *lua.LState) int {
			name := L.CheckString(1)
			val, err := w.Latest(name)
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
				return 2
			}
			L.Push(lua.LString(val))
			return 1
		}))

		for _, fn := range []struct {
			name   string
			call   func(string, time.Duration) (float64, error)
			maxDur time.Duration
		}{
			{"avg", w.Avg, maxAggDuration},
			{"max", w.Max, maxAggDuration},
			{"min", w.Min, maxAggDuration},
			{"sum", w.Sum, maxAggDuration},
			{"stddev", w.Stddev, maxAggDuration},
			{"count", w.Count, maxAggDuration},
			{"rate", w.Rate, maxAggDuration},
			{"first", w.First, maxRawDuration},
			{"delta", w.Delta, maxRawDuration},
			{"slope", w.Slope, maxRawDuration},
		} {
			fn := fn
			mod.RawSetString(fn.name, L.NewFunction(func(L *lua.LState) int {
				name := L.CheckString(1)
				durStr := L.CheckString(2)
				dur, err := parseDuration(durStr)
				if err != nil {
					L.Push(lua.LNil)
					L.Push(lua.LString("invalid duration: " + durStr))
					return 2
				}
				if dur > fn.maxDur || dur <= 0 {
					L.Push(lua.LNil)
					L.Push(lua.LString(fmt.Sprintf("duration must be between 0 and %s", fn.maxDur)))
					return 2
				}
				val, err := fn.call(name, dur)
				if err != nil {
					L.Push(lua.LNil)
					L.Push(lua.LString(err.Error()))
					return 2
				}
				L.Push(lua.LNumber(val))
				return 1
			}))
		}

		mod.RawSetString("percentile", L.NewFunction(func(L *lua.LState) int {
			name := L.CheckString(1)
			durStr := L.CheckString(2)
			p := float64(L.CheckNumber(3))
			dur, err := parseDuration(durStr)
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString("invalid duration: " + durStr))
				return 2
			}
			if dur > maxRawDuration || dur <= 0 {
				L.Push(lua.LNil)
				L.Push(lua.LString(fmt.Sprintf("duration must be between 0 and %s", maxRawDuration)))
				return 2
			}
			val, err := w.Percentile(name, dur, p)
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
				return 2
			}
			L.Push(lua.LNumber(val))
			return 1
		}))

		mod.RawSetString("previous", L.NewFunction(func(L *lua.LState) int {
			name := L.CheckString(1)
			val, err := w.Previous(name)
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
				return 2
			}
			L.Push(lua.LString(val))
			return 1
		}))

		mod.RawSetString("describe", L.NewFunction(func(L *lua.LState) int {
			name := L.CheckString(1)
			n := L.OptInt(2, defaultDescribeRuns)
			runs, err := w.Describe(name, n)
			if err != nil {
				L.Push(lua.LNil)
				L.Push(lua.LString(err.Error()))
				return 2
			}
			tbl := L.NewTable()
			for _, r := range runs {
				row := L.NewTable()
				row.RawSetString("ran_at", lua.LString(r.RanAt.Format(time.RFC3339)))
				row.RawSetString("exit_code", lua.LNumber(r.ExitCode))
				row.RawSetString("duration_ms", lua.LNumber(r.DurationMs))
				row.RawSetString("tag", lua.LString(r.Tag))
				if r.Value != nil {
					row.RawSetString("value", lua.LNumber(*r.Value))
				}
				if r.Text != nil {
					row.RawSetString("text", lua.LString(*r.Text))
				}
				row.RawSetString("stdout", lua.LString(r.Stdout))
				row.RawSetString("stderr", lua.LString(r.Stderr))
				tbl.Append(row)
			}
			L.Push(tbl)
			return 1
		}))

		registerWeftLuaPredicates(L, mod, w)

		// A numeric function on a text cron raises a Lua error rather than
		// returning nil, err (or false, for a predicate): it is a mistake
		// in the script, not missing data, and must not read as one.
		mod.ForEach(func(k, v lua.LValue) {
			name := k.String()
			fn, ok := v.(*lua.LFunction)
			if !ok || !store.NumericWeftFunc(name) {
				return
			}
			orig := fn.GFunction
			mod.RawSetString(name, L.NewFunction(func(L *lua.LState) int {
				if err := w.CheckNumeric("weft."+name, L.CheckString(1)); err != nil {
					L.RaiseError("%s", err.Error())
				}
				return orig(L)
			}))
		})

		L.SetGlobal("weft", mod)
	}
}

// registerWeftLuaPredicates registers boolean wrappers over the value
// primitives — weft.is_above / is_below / is_absent / has_changed /
// is_rising / is_falling / is_outlier / is_seasonal. Each returns a
// Lua bool so composition uses native `and`/`or`/`not`:
//
//	if weft.is_above("error-rate", 50) and weft.is_absent("canary-ping") then
//	  print(1)
//	else
//	  print(0)
//	end
//
// On absent data or computation errors, predicates return false (not
// nil+err) — "we can't say yes, so it doesn't fire" matches how
// alerts skip on missing history.
func registerWeftLuaPredicates(L *lua.LState, mod *lua.LTable, w weftQueryFuncs) {
	latestFloat := func(name string) (float64, bool) {
		s, err := w.Latest(name)
		if err != nil || s == "" {
			return 0, false
		}
		f, perr := parseLuaFloat(s)
		if perr != nil {
			return 0, false
		}
		return f, true
	}

	mod.RawSetString("is_above", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		threshold := float64(L.CheckNumber(2))
		if L.GetTop() >= 3 {
			dur, ok := optLuaDuration(L, 3)
			if !ok {
				L.Push(lua.LFalse)
				return 1
			}
			useAvg := L.OptBool(4, false)
			var v float64
			var err error
			if useAvg {
				v, err = w.Avg(name, dur)
			} else {
				v, err = w.Min(name, dur)
			}
			if err != nil {
				L.Push(lua.LFalse)
				return 1
			}
			L.Push(lua.LBool(v > threshold))
			return 1
		}
		v, ok := latestFloat(name)
		if !ok {
			L.Push(lua.LFalse)
			return 1
		}
		L.Push(lua.LBool(v > threshold))
		return 1
	}))

	mod.RawSetString("is_below", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		threshold := float64(L.CheckNumber(2))
		if L.GetTop() >= 3 {
			dur, ok := optLuaDuration(L, 3)
			if !ok {
				L.Push(lua.LFalse)
				return 1
			}
			useAvg := L.OptBool(4, false)
			var v float64
			var err error
			if useAvg {
				v, err = w.Avg(name, dur)
			} else {
				v, err = w.Max(name, dur)
			}
			if err != nil {
				L.Push(lua.LFalse)
				return 1
			}
			L.Push(lua.LBool(v < threshold))
			return 1
		}
		v, ok := latestFloat(name)
		if !ok {
			L.Push(lua.LFalse)
			return 1
		}
		L.Push(lua.LBool(v < threshold))
		return 1
	}))

	mod.RawSetString("is_absent", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		if L.GetTop() >= 2 {
			dur, ok := optLuaDuration(L, 2)
			if !ok {
				L.Push(lua.LFalse)
				return 1
			}
			absent, err := w.Absent(name, dur)
			L.Push(lua.LBool(err == nil && absent))
			return 1
		}
		absent, err := w.Absent(name, 0)
		L.Push(lua.LBool(err == nil && absent))
		return 1
	}))

	mod.RawSetString("has_changed", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		latest, err := w.Latest(name)
		if err != nil {
			L.Push(lua.LFalse)
			return 1
		}
		prev, err := w.Previous(name)
		if err != nil || prev == "" {
			L.Push(lua.LFalse)
			return 1
		}
		L.Push(lua.LBool(latest != prev))
		return 1
	}))

	mod.RawSetString("is_rising", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LBool(evalTrendLua(L, w, true)))
		return 1
	}))
	mod.RawSetString("is_falling", L.NewFunction(func(L *lua.LState) int {
		L.Push(lua.LBool(evalTrendLua(L, w, false)))
		return 1
	}))

	mod.RawSetString("is_outlier", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		nSigma := float64(L.CheckNumber(2))
		dur, ok := optLuaDuration(L, 3)
		if !ok {
			L.Push(lua.LFalse)
			return 1
		}
		direction := L.OptString(4, "")
		current, ok := latestFloat(name)
		if !ok {
			L.Push(lua.LFalse)
			return 1
		}
		avg, err := w.Avg(name, dur)
		if err != nil {
			L.Push(lua.LFalse)
			return 1
		}
		stddev, err := w.Stddev(name, dur)
		if err != nil {
			L.Push(lua.LFalse)
			return 1
		}
		floor := 0.01 * absFloat(avg)
		if floor < 1e-9 {
			floor = 1e-9
		}
		if stddev < floor {
			L.Push(lua.LFalse)
			return 1
		}
		delta := current - avg
		switch direction {
		case "above":
			L.Push(lua.LBool(delta > nSigma*stddev))
		case "below":
			L.Push(lua.LBool(-delta > nSigma*stddev))
		default:
			L.Push(lua.LBool(absFloat(delta) > nSigma*stddev))
		}
		return 1
	}))

	mod.RawSetString("is_seasonal", L.NewFunction(func(L *lua.LState) int {
		name := L.CheckString(1)
		nSigma := float64(L.CheckNumber(2))
		period, ok := optLuaDuration(L, 3)
		if !ok {
			L.Push(lua.LFalse)
			return 1
		}
		window, ok := optLuaDuration(L, 4)
		if !ok {
			L.Push(lua.LFalse)
			return 1
		}
		current, ok := latestFloat(name)
		if !ok {
			L.Push(lua.LFalse)
			return 1
		}
		stats, err := w.SeasonalStats(name, period, window)
		if err != nil || stats.Samples < 4 {
			L.Push(lua.LFalse)
			return 1
		}
		floor := 0.01 * absFloat(stats.Avg)
		if floor < 1e-9 {
			floor = 1e-9
		}
		if stats.Stddev < floor {
			L.Push(lua.LFalse)
			return 1
		}
		L.Push(lua.LBool(absFloat(current-stats.Avg) > nSigma*stats.Stddev))
		return 1
	}))
}

func evalTrendLua(L *lua.LState, w weftQueryFuncs, rising bool) bool {
	name := L.CheckString(1)
	pct := float64(L.OptNumber(2, 0)) / 100.0
	latestStr, err := w.Latest(name)
	if err != nil || latestStr == "" {
		return false
	}
	current, err := parseLuaFloat(latestStr)
	if err != nil {
		return false
	}
	var baseline float64
	if L.GetTop() >= 3 {
		dur, ok := optLuaDuration(L, 3)
		if !ok {
			return false
		}
		v, err := w.Avg(name, dur)
		if err != nil {
			return false
		}
		baseline = v
	} else {
		prev, err := w.Previous(name)
		if err != nil || prev == "" {
			return false
		}
		v, err := parseLuaFloat(prev)
		if err != nil {
			return false
		}
		baseline = v
	}
	if baseline == 0 {
		return false
	}
	pctChange := (current - baseline) / baseline
	if rising {
		return pctChange >= pct
	}
	return -pctChange >= pct
}

func optLuaDuration(L *lua.LState, idx int) (time.Duration, bool) {
	s := L.CheckString(idx)
	d, err := parseDuration(s)
	if err != nil || d <= 0 {
		return 0, false
	}
	if d > maxAggDuration {
		return 0, false
	}
	return d, true
}

func parseLuaFloat(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}
