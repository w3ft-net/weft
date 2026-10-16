package coordinator

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	luapkg "github.com/graybeardcorp/warp/pkg/lua"
	"github.com/graybeardcorp/weft/store"
	lua "github.com/yuin/gopher-lua"
	"gorm.io/gorm"
)

// maxAggDuration is the cap for true aggregates (avg, max, min, sum,
// stddev, count, percentile, rate) — funcs that touch every result
// in the window. Bounds compute cost.
const maxAggDuration = 24 * time.Hour

// maxEndpointDuration is the cap for endpoint funcs (first, delta,
// slope) that read O(1) anchor results — the chronologically first
// result still in the window plus the latest. They can safely span
// a longer window, useful for trend / fill-prediction queries.
const maxEndpointDuration = 14 * 24 * time.Hour

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
		n, err := w.Count(watchName, window)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return true, nil
		}
		if err != nil {
			return false, err
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

func (w *WeftFuncs) Avg(watchName string, dur time.Duration) (float64, error) {
	vals, err := w.numericResults(watchName, dur)
	if err != nil {
		return 0, err
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("weft::avg: no numeric results for %q", watchName)
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals)), nil
}

func (w *WeftFuncs) Max(watchName string, dur time.Duration) (float64, error) {
	vals, err := w.numericResults(watchName, dur)
	if err != nil {
		return 0, err
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("weft::max: no numeric results for %q", watchName)
	}
	max := vals[0]
	for _, v := range vals[1:] {
		if v > max {
			max = v
		}
	}
	return max, nil
}

func (w *WeftFuncs) Min(watchName string, dur time.Duration) (float64, error) {
	vals, err := w.numericResults(watchName, dur)
	if err != nil {
		return 0, err
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("weft::min: no numeric results for %q", watchName)
	}
	min := vals[0]
	for _, v := range vals[1:] {
		if v < min {
			min = v
		}
	}
	return min, nil
}

func (w *WeftFuncs) Sum(watchName string, dur time.Duration) (float64, error) {
	vals, err := w.numericResults(watchName, dur)
	if err != nil {
		return 0, err
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("weft::sum: no numeric results for %q", watchName)
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum, nil
}

func (w *WeftFuncs) Stddev(watchName string, dur time.Duration) (float64, error) {
	vals, err := w.numericResults(watchName, dur)
	if err != nil {
		return 0, err
	}
	if len(vals) < 2 {
		return 0, fmt.Errorf("weft::stddev: need at least 2 results for %q", watchName)
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var variance float64
	for _, v := range vals {
		d := v - mean
		variance += d * d
	}
	variance /= float64(len(vals))
	return math.Sqrt(variance), nil
}

// First returns the chronologically oldest numeric stdout still in
// the window — the anchor for delta/slope. Cap is the longer
// maxEndpointDuration since this reads a single result, not an
// aggregate.
func (w *WeftFuncs) First(watchName string, dur time.Duration) (float64, error) {
	vals, err := w.numericResultsCap(watchName, dur, maxEndpointDuration)
	if err != nil {
		return 0, err
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("weft::first: no numeric results for %q", watchName)
	}
	// QueryCronResults returns DESC (newest first); oldest is the last element.
	return vals[len(vals)-1], nil
}

// Count returns the number of results recorded for the watch within
// the window — numeric or not, since "how many times did the watch
// fire" is the natural question. Returns float for interface uniformity.
func (w *WeftFuncs) Count(watchName string, dur time.Duration) (float64, error) {
	if dur > maxAggDuration {
		return 0, fmt.Errorf("duration %s exceeds maximum of %s", dur, maxAggDuration)
	}
	if dur <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	since := time.Now().Add(-dur)
	results, err := w.Store.QueryCronResults(w.OrgID, watchName, since)
	if err != nil {
		return 0, fmt.Errorf("weft::count: cron %q: %w", watchName, err)
	}
	return float64(len(results)), nil
}

// Percentile returns the p-th percentile (p in 0..100) of numeric
// stdout values in the window using linear interpolation between
// adjacent ranks (R-7 / Excel / numpy default). p=0 is min, p=50 is
// median, p=100 is max.
func (w *WeftFuncs) Percentile(watchName string, dur time.Duration, p float64) (float64, error) {
	if p < 0 || p > 100 {
		return 0, fmt.Errorf("weft::percentile: p must be in [0, 100], got %v", p)
	}
	vals, err := w.numericResults(watchName, dur)
	if err != nil {
		return 0, err
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("weft::percentile: no numeric results for %q", watchName)
	}
	sort.Float64s(vals)
	if len(vals) == 1 {
		return vals[0], nil
	}
	rank := p / 100.0 * float64(len(vals)-1)
	lo := int(rank)
	frac := rank - float64(lo)
	if lo+1 >= len(vals) {
		return vals[len(vals)-1], nil
	}
	return vals[lo] + frac*(vals[lo+1]-vals[lo]), nil
}

// Delta returns latest_numeric - first_numeric over the window.
// Composite of weft::latest and weft::first; implemented natively for
// performance and to share the single-fetch slice. Cap is
// maxEndpointDuration since the underlying anchors are O(1).
func (w *WeftFuncs) Delta(watchName string, dur time.Duration) (float64, error) {
	vals, err := w.numericResultsCap(watchName, dur, maxEndpointDuration)
	if err != nil {
		return 0, err
	}
	if len(vals) < 2 {
		return 0, fmt.Errorf("weft::delta: need at least 2 numeric results for %q", watchName)
	}
	// vals[0] is newest, vals[len-1] is oldest.
	return vals[0] - vals[len(vals)-1], nil
}

// Slope returns Delta / dur.Seconds() — the average rate of change
// per second across the window. Caller multiplies for other units.
// Cap is maxEndpointDuration.
func (w *WeftFuncs) Slope(watchName string, dur time.Duration) (float64, error) {
	d, err := w.Delta(watchName, dur)
	if err != nil {
		return 0, err
	}
	return d / dur.Seconds(), nil
}

// Rate returns Sum / dur.Seconds() — events-per-second when the
// watch's stdout is a per-period count. Caller multiplies for
// per-minute / per-hour. Cap is maxAggDuration since this is a true
// aggregate over every result.
func (w *WeftFuncs) Rate(watchName string, dur time.Duration) (float64, error) {
	s, err := w.Sum(watchName, dur)
	if err != nil {
		return 0, err
	}
	return s / dur.Seconds(), nil
}

// SeasonalStats returns avg/stddev/samples of the watch's history
// restricted to "same time of period" — e.g. for period=1d and
// window=4w it computes over the readings from this hour on each of
// the last four days. Used by the seasonal predicate and (future)
// alert seasonal condition. Cap is maxEndpointDuration since the
// window can legitimately span multiple weeks. The result type lives
// in warp's lua package to keep the WeftFuncs interface standalone.
func (w *WeftFuncs) SeasonalStats(watchName string, period, window time.Duration) (SeasonalStatsResult, error) {
	if window > maxEndpointDuration {
		return SeasonalStatsResult{}, fmt.Errorf("weft::is_seasonal: window %s exceeds maximum of %s", window, maxEndpointDuration)
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

// numericResults fetches results in a time range and parses numeric stdout values.
func (w *WeftFuncs) numericResults(watchName string, dur time.Duration) ([]float64, error) {
	return w.numericResultsCap(watchName, dur, maxAggDuration)
}

// numericResultsCap is numericResults with a caller-supplied cap, so
// endpoint funcs can use a longer window than true aggregates.
func (w *WeftFuncs) numericResultsCap(watchName string, dur time.Duration, maxDur time.Duration) ([]float64, error) {
	if dur > maxDur {
		return nil, fmt.Errorf("duration %s exceeds maximum of %s", dur, maxDur)
	}
	if dur <= 0 {
		return nil, fmt.Errorf("duration must be positive")
	}
	since := time.Now().Add(-dur)
	results, err := w.Store.QueryCronResults(w.OrgID, watchName, since)
	if err != nil {
		return nil, fmt.Errorf("cron %q: %w", watchName, err)
	}
	var vals []float64
	for _, r := range results {
		s := strings.TrimSpace(r.Stdout)
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			vals = append(vals, v)
		}
	}
	return vals, nil
}

// ParseAggDuration parses a duration string (30m, 1h, 6h, 24h, 1d) for aggregates.
func ParseAggDuration(s string) (time.Duration, error) {
	d, err := parseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g., 30m, 1h, 6h, 1d)", s)
	}
	if d > maxAggDuration {
		return 0, fmt.Errorf("duration %s exceeds maximum of %s", d, maxAggDuration)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return d, nil
}

// ParseEndpointDuration parses a duration string for endpoint funcs
// (first, delta, slope), allowing windows up to maxEndpointDuration.
func ParseEndpointDuration(s string) (time.Duration, error) {
	d, err := parseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g., 1h, 24h, 7d, 2w)", s)
	}
	if d > maxEndpointDuration {
		return 0, fmt.Errorf("duration %s exceeds maximum of %s", d, maxEndpointDuration)
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
			{"first", w.First, maxEndpointDuration},
			{"delta", w.Delta, maxEndpointDuration},
			{"slope", w.Slope, maxEndpointDuration},
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
			if dur > maxAggDuration || dur <= 0 {
				L.Push(lua.LNil)
				L.Push(lua.LString(fmt.Sprintf("duration must be between 0 and %s", maxAggDuration)))
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

		registerWeftLuaPredicates(L, mod, w)

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
	if d > maxEndpointDuration {
		return 0, false
	}
	return d, true
}

func parseLuaFloat(s string) (float64, error) {
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}
