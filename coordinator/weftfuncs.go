package coordinator

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	luapkg "github.com/kevlarcon/warp/pkg/lua"
	"github.com/kevlarcon/weft/store"
)

// Compile-time check that WeftFuncs implements luapkg.WeftFuncs.
var _ luapkg.WeftFuncs = (*WeftFuncs)(nil)

// maxAggDuration is the cap for true aggregates (avg, max, min, sum,
// stddev, count, percentile, rate) — funcs that touch every result
// in the window. Bounds compute cost.
const maxAggDuration = 24 * time.Hour

// maxEndpointDuration is the cap for endpoint funcs (first, delta,
// slope) that read O(1) anchor results — the chronologically first
// result still in the window plus the latest. They can safely span
// a longer window, useful for trend / fill-prediction queries.
const maxEndpointDuration = 14 * 24 * time.Hour

// WeftFuncs provides cron result query functions for /bin/ scripts.
// Both bash (ExternalCmdFunc) and Lua (weft.* table) use this.
type WeftFuncs struct {
	Store      *store.Store
	CustomerID uint
}

func (w *WeftFuncs) Latest(watchName string) (string, error) {
	r, err := w.Store.LatestCronResult(w.CustomerID, watchName)
	if err != nil {
		return "", fmt.Errorf("weft::latest: watch %q: %w", watchName, err)
	}
	return strings.TrimSpace(r.Stdout), nil
}

// Previous returns the trimmed stdout of the second-most-recent
// result for a named watch — the value just before Latest. Used by
// changed/rising/falling predicates for prev-tick semantics.
// Returns "" with no error when fewer than 2 results exist.
func (w *WeftFuncs) Previous(watchName string) (string, error) {
	cw, err := w.Store.GetCronWatch(w.CustomerID, watchName)
	if err != nil {
		return "", fmt.Errorf("weft::previous: watch %q: %w", watchName, err)
	}
	results, err := w.Store.ListCronResults(cw.ID, 2, "")
	if err != nil {
		return "", fmt.Errorf("weft::previous: watch %q: %w", watchName, err)
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
	results, err := w.Store.QueryCronResults(w.CustomerID, watchName, since)
	if err != nil {
		return 0, fmt.Errorf("weft::count: watch %q: %w", watchName, err)
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
func (w *WeftFuncs) SeasonalStats(watchName string, period, window time.Duration) (luapkg.SeasonalStatsResult, error) {
	if window > maxEndpointDuration {
		return luapkg.SeasonalStatsResult{}, fmt.Errorf("weft::is_seasonal: window %s exceeds maximum of %s", window, maxEndpointDuration)
	}
	if window <= 0 || period <= 0 {
		return luapkg.SeasonalStatsResult{}, fmt.Errorf("weft::is_seasonal: window and period must be positive")
	}
	if period >= window {
		return luapkg.SeasonalStatsResult{}, fmt.Errorf("weft::is_seasonal: window must be larger than period")
	}
	avg, stddev, samples, err := w.Store.SeasonalCronStats(w.CustomerID, watchName, period, window)
	if err != nil {
		return luapkg.SeasonalStatsResult{}, fmt.Errorf("weft::is_seasonal: watch %q: %w", watchName, err)
	}
	return luapkg.SeasonalStatsResult{Avg: avg, Stddev: stddev, Samples: samples}, nil
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
	results, err := w.Store.QueryCronResults(w.CustomerID, watchName, since)
	if err != nil {
		return nil, fmt.Errorf("watch %q: %w", watchName, err)
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

// ParseAggDuration parses a duration string (30m, 1h, 6h, 24h) for aggregates.
func ParseAggDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g., 30m, 1h, 6h)", s)
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
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q (use e.g., 1h, 24h, 7d-equivalent in hours)", s)
	}
	if d > maxEndpointDuration {
		return 0, fmt.Errorf("duration %s exceeds maximum of %s", d, maxEndpointDuration)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive")
	}
	return d, nil
}
