package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
)

// RateShape is a deterministic function of elapsed seconds → rate
// (or ratio, when used for ERROR_RATIO). Ported one-for-one from
// dev/demo/generator.py so dev and prod emit interchangeable
// streams.
//
// Forms:
//
//	const:R                  flat at R
//	sine:base,amp,period     base + amp*(1+sin(2π·t/period))/2
//	sawtooth:base,slope,reset  ramp up, wrap to base at reset
//	monotonic:base,slope[,reset]  unbounded climb when reset omitted;
//	                              same as sawtooth otherwise
type RateShape struct {
	Kind string
	// Numeric params; unused slots are zero. Reset is *float64 because
	// monotonic distinguishes "unset" from "0".
	Rate, Base, Amp, Period, Slope float64
	Reset                          *float64
}

func (s RateShape) At(t float64) float64 {
	switch s.Kind {
	case "const":
		return s.Rate
	case "sine":
		return s.Base + s.Amp*(1+math.Sin(2*math.Pi*t/s.Period))/2
	case "sawtooth":
		return rampWithReset(t, s.Base, s.Slope, *s.Reset)
	case "monotonic":
		if s.Reset == nil {
			return s.Base + s.Slope*t
		}
		return rampWithReset(t, s.Base, s.Slope, *s.Reset)
	}
	// Caller guarantees a valid Kind via ParseShape; falling through
	// would mean a programming error, so make it loud.
	panic(fmt.Sprintf("loom-producer: unknown shape kind %q", s.Kind))
}

func rampWithReset(t, base, slope, reset float64) float64 {
	if slope <= 0 {
		return base
	}
	period := (reset - base) / slope
	if period <= 0 {
		return base
	}
	within := t - period*math.Floor(t/period)
	return base + slope*within
}

// TokensForStep is the Bernoulli-fractional emit count: floor(rate*dt) plus
// a coin flip on the fractional remainder. Exposed so the table tests can
// integrate over a window without spinning a real loop.
func TokensForStep(rng *rand.Rand, rate, dt float64) int {
	expected := rate * dt
	whole := int(expected)
	frac := expected - float64(whole)
	if rng.Float64() < frac {
		whole++
	}
	return whole
}

func ParseShape(spec string) (RateShape, error) {
	idx := strings.IndexByte(spec, ':')
	if idx < 0 {
		return RateShape{}, fmt.Errorf("shape needs colon: %q", spec)
	}
	kind, rest := spec[:idx], spec[idx+1:]
	rawParts := strings.Split(rest, ",")
	var parts []string
	for _, p := range rawParts {
		p = strings.TrimSpace(p)
		if p != "" {
			parts = append(parts, p)
		}
	}
	nums := make([]float64, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return RateShape{}, fmt.Errorf("non-numeric param in %q: %w", spec, err)
		}
		nums = append(nums, n)
	}
	switch kind {
	case "const":
		if len(nums) != 1 {
			return RateShape{}, fmt.Errorf("const takes exactly 1 param: const:R")
		}
		return RateShape{Kind: "const", Rate: nums[0]}, nil
	case "sine":
		if len(nums) != 3 {
			return RateShape{}, fmt.Errorf("sine takes 3 params: sine:base,amp,period")
		}
		return RateShape{Kind: "sine", Base: nums[0], Amp: nums[1], Period: nums[2]}, nil
	case "sawtooth":
		if len(nums) != 3 {
			return RateShape{}, fmt.Errorf("sawtooth takes 3 params: sawtooth:base,slope,reset")
		}
		reset := nums[2]
		return RateShape{Kind: "sawtooth", Base: nums[0], Slope: nums[1], Reset: &reset}, nil
	case "monotonic":
		if len(nums) != 2 && len(nums) != 3 {
			return RateShape{}, fmt.Errorf("monotonic takes 2 or 3 params: monotonic:base,slope[,reset]")
		}
		out := RateShape{Kind: "monotonic", Base: nums[0], Slope: nums[1]}
		if len(nums) == 3 {
			r := nums[2]
			out.Reset = &r
		}
		return out, nil
	}
	return RateShape{}, fmt.Errorf("unknown shape: %s", kind)
}

// ParseErrorRatio accepts either a bare float (constant) or a full
// shape spec. Returning a RateShape lets the main loop sample
// error_ratio as a function of time, matching the Python version's
// behavior so the loom-api-prod 4%→14%/900s sweep keeps working.
func ParseErrorRatio(spec string) (RateShape, error) {
	if f, err := strconv.ParseFloat(strings.TrimSpace(spec), 64); err == nil {
		return RateShape{Kind: "const", Rate: f}, nil
	}
	return ParseShape(spec)
}
