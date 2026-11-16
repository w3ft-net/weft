package ingest

import (
	"math"
	"strconv"
	"time"
)

// durationMode is how a schema's duration_unit reads the extracted text.
type durationMode struct {
	mult int64 // nanoseconds per unit of a bare number; 0 when the text carries its own units
	text bool  // the text is a unit-suffixed duration ("5.01s", "1m2s")
}

// durationUnitMode resolves a schema's duration_unit; ok is false for a value
// the schema does not define.
func durationUnitMode(unit string) (m durationMode, ok bool) {
	switch unit {
	case "", "ns":
		return durationMode{mult: 1}, true
	case "us":
		return durationMode{mult: int64(time.Microsecond)}, true
	case "ms":
		return durationMode{mult: int64(time.Millisecond)}, true
	case "s":
		return durationMode{mult: int64(time.Second)}, true
	case "text":
		return durationMode{text: true}, true
	}
	return durationMode{}, false
}

// unitVariants lists the units to try for a duration rule the model proposed
// with unit, best first. Whether the captured text carries its own unit is a
// fact the parser can check; only the scale of a bare number is the model's
// call. So a "text" answer that fails on bare numbers falls back to whole
// nanoseconds, and a bare-unit answer that fails on suffixed text falls back
// to "text".
func unitVariants(unit string) []string {
	if unit == "text" {
		return []string{"text", "ns"}
	}
	return []string{unit, "text"}
}

// parse converts extracted duration text to integer nanoseconds. ok is false
// for text that is not a non-negative duration in this mode.
func (m durationMode) parse(s string) (ns int64, ok bool) {
	if m.text {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return 0, false
		}
		return int64(d), true
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return 0, false
		}
		return n * m.mult, true
	}
	if m.mult == 1 {
		return 0, false // nanoseconds are a whole count
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return 0, false
	}
	return int64(math.Round(f * float64(m.mult))), true
}
