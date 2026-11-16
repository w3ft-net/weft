package cronstats

import (
	"math"
	"sort"
	"time"

	"github.com/w3ft-net/weft/backends/cron"
)

// Percentile is the p-th percentile (p in 0..100) of values, with linear
// interpolation between adjacent ranks (R-7: Excel, numpy's default, Postgres's
// percentile_cont). p=0 is the minimum, 50 the median, 100 the maximum. ok is
// false when there are no values. values is not modified.
func Percentile(values []float64, p float64) (v float64, ok bool) {
	if len(values) == 0 {
		return 0, false
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	rank := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return s[lo], true
	}
	return s[lo] + (s[hi]-s[lo])*(rank-float64(lo)), true
}

// Seasonal is the average, sample standard deviation and count of the points
// taken at the same time of a period as now: those whose age is within a
// tolerance of a whole number of periods. The tolerance is a twentieth of the
// period, at most five minutes. It is the baseline a seasonal alert compares
// the current value to ("this hour, on each of the last four days").
type Seasonal struct {
	Avg     float64
	Stddev  float64
	Samples int
}

// SeasonalBaseline computes Seasonal over points, which the caller bounded to
// its window. Stddev is 0 for fewer than two samples.
func SeasonalBaseline(points []cron.Point, now time.Time, period time.Duration) Seasonal {
	tol := period / 20
	if tol > 5*time.Minute {
		tol = 5 * time.Minute
	}
	periodSec, tolSec := period.Seconds(), tol.Seconds()
	var n int
	var sum float64
	var kept []float64
	for _, p := range points {
		// The remainder keeps the age's sign, as Postgres's MOD does: a point in
		// the future (negative age) is always within tolerance.
		rem := math.Mod(now.Sub(p.At).Seconds(), periodSec)
		if math.Min(rem, periodSec-rem) > tolSec {
			continue
		}
		n++
		sum += p.Value
		kept = append(kept, p.Value)
	}
	if n == 0 {
		return Seasonal{}
	}
	out := Seasonal{Avg: sum / float64(n), Samples: n}
	if n > 1 {
		var ss float64
		for _, v := range kept {
			d := v - out.Avg
			ss += d * d
		}
		out.Stddev = math.Sqrt(ss / float64(n-1))
	}
	return out
}
