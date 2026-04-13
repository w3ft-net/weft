package coordinator

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	luapkg "github.com/kevlarcon/warp/pkg/lua"
	"github.com/kevlarcon/weft/store"
)

// Compile-time check that WeftFuncs implements luapkg.WeftFuncs.
var _ luapkg.WeftFuncs = (*WeftFuncs)(nil)

const maxAggDuration = 24 * time.Hour

// WeftFuncs provides cron result query functions for /bin/ scripts.
// Both bash (ExternalCmdFunc) and Lua (weft.* table) use this.
type WeftFuncs struct {
	Store      *store.Store
	CustomerID uint
}

func (w *WeftFuncs) Latest(watchName string) (string, error) {
	r, err := w.Store.LatestCronResult(w.CustomerID, watchName)
	if err != nil {
		return "", fmt.Errorf("weft.latest: watch %q: %w", watchName, err)
	}
	return strings.TrimSpace(r.Stdout), nil
}

func (w *WeftFuncs) Avg(watchName string, dur time.Duration) (float64, error) {
	vals, err := w.numericResults(watchName, dur)
	if err != nil {
		return 0, err
	}
	if len(vals) == 0 {
		return 0, fmt.Errorf("weft.avg: no numeric results for %q", watchName)
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
		return 0, fmt.Errorf("weft.max: no numeric results for %q", watchName)
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
		return 0, fmt.Errorf("weft.min: no numeric results for %q", watchName)
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
		return 0, fmt.Errorf("weft.sum: no numeric results for %q", watchName)
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
		return 0, fmt.Errorf("weft.stddev: need at least 2 results for %q", watchName)
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

// numericResults fetches results in a time range and parses numeric stdout values.
func (w *WeftFuncs) numericResults(watchName string, dur time.Duration) ([]float64, error) {
	if dur > maxAggDuration {
		return nil, fmt.Errorf("duration %s exceeds maximum of %s", dur, maxAggDuration)
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
