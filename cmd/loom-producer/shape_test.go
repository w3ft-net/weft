package main

import (
	"math"
	"math/rand/v2"
	"testing"
)

func TestParseShape(t *testing.T) {
	tests := []struct {
		spec string
		want RateShape
	}{
		{"const:8", RateShape{Kind: "const", Rate: 8}},
		{"sine:20,40,600", RateShape{Kind: "sine", Base: 20, Amp: 40, Period: 600}},
		{"sawtooth:2,0.05,12", RateShape{Kind: "sawtooth", Base: 2, Slope: 0.05, Reset: ptr(12.0)}},
		{"monotonic:5,0.05,40", RateShape{Kind: "monotonic", Base: 5, Slope: 0.05, Reset: ptr(40.0)}},
		{"monotonic:1,0.02", RateShape{Kind: "monotonic", Base: 1, Slope: 0.02, Reset: nil}},
	}
	for _, tc := range tests {
		got, err := ParseShape(tc.spec)
		if err != nil {
			t.Fatalf("ParseShape(%q): %v", tc.spec, err)
		}
		if got.Kind != tc.want.Kind || got.Rate != tc.want.Rate ||
			got.Base != tc.want.Base || got.Amp != tc.want.Amp ||
			got.Period != tc.want.Period || got.Slope != tc.want.Slope ||
			!fpeq(got.Reset, tc.want.Reset) {
			t.Errorf("ParseShape(%q): got %+v want %+v", tc.spec, got, tc.want)
		}
	}
}

func TestParseShapeErrors(t *testing.T) {
	for _, spec := range []string{
		"bogus:1,2,3",
		"sine:20,40",
		"const:1,2",
		"no_colon",
	} {
		if _, err := ParseShape(spec); err == nil {
			t.Errorf("ParseShape(%q): expected error", spec)
		}
	}
}

func TestRateAtConstFlat(t *testing.T) {
	s, _ := ParseShape("const:7")
	for _, x := range []float64{0, 100, 1e9} {
		if got := s.At(x); got != 7 {
			t.Errorf("const At(%v) = %v want 7", x, got)
		}
	}
}

func TestRateAtSineOscillates(t *testing.T) {
	s, _ := ParseShape("sine:20,40,600")
	cases := []struct {
		t, want float64
	}{
		{0, 40.0},   // midpoint
		{150, 60.0}, // max
		{300, 40.0}, // midpoint
		{450, 20.0}, // min
	}
	for _, c := range cases {
		if got := s.At(c.t); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("sine At(%v) = %v want %v", c.t, got, c.want)
		}
	}
}

func TestRateAtSawtoothClimbsThenResets(t *testing.T) {
	// base=2, slope=0.5, reset=10 → period = 16s
	s, _ := ParseShape("sawtooth:2,0.5,10")
	checks := []struct {
		t, want float64
	}{
		{0, 2.0},
		{8, 6.0},
		{16, 2.0}, // wrap back to base at boundary
		{24, 6.0}, // second period
	}
	for _, c := range checks {
		if got := s.At(c.t); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("sawtooth At(%v) = %v want %v", c.t, got, c.want)
		}
	}
	if got := s.At(15.999); got >= 10.0 {
		t.Errorf("sawtooth At(15.999) = %v; expected < 10", got)
	}
}

func TestRateAtMonotonicWithResetMatchesSawtooth(t *testing.T) {
	a, _ := ParseShape("monotonic:0,0.1,30")
	b, _ := ParseShape("sawtooth:0,0.1,30")
	for _, x := range []float64{0, 50, 150, 299, 300, 301, 1000} {
		if math.Abs(a.At(x)-b.At(x)) > 1e-9 {
			t.Errorf("monotonic/sawtooth divergence at t=%v: %v vs %v", x, a.At(x), b.At(x))
		}
	}
}

func TestRateAtMonotonicUnbounded(t *testing.T) {
	s, _ := ParseShape("monotonic:1,0.02")
	cases := []struct {
		t, want float64
	}{
		{0, 1.0},
		{60, 2.2},
		{3600, 73.0},
	}
	for _, c := range cases {
		if got := s.At(c.t); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("monotonic At(%v) = %v want %v", c.t, got, c.want)
		}
	}
	if got := s.At(1e6); got <= 100 {
		t.Errorf("monotonic At(1e6) = %v; expected > 100", got)
	}
}

func TestParseErrorRatio(t *testing.T) {
	// Bare float → constant ratio.
	s, err := ParseErrorRatio("0.08")
	if err != nil {
		t.Fatalf("ParseErrorRatio(\"0.08\"): %v", err)
	}
	if s.Kind != "const" || math.Abs(s.At(0)-0.08) > 1e-9 || math.Abs(s.At(9999)-0.08) > 1e-9 {
		t.Errorf("bare-float ratio: got %+v", s)
	}

	// Shape spec → time-varying ratio. base=0.04 amp=0.10 period=600 →
	// At(0) = 0.09, At(150) = 0.14, At(450) = 0.04.
	s, err = ParseErrorRatio("sine:0.04,0.10,600")
	if err != nil {
		t.Fatalf("ParseErrorRatio sine: %v", err)
	}
	checks := []struct {
		t, want float64
	}{
		{0, 0.09},
		{150, 0.14},
		{450, 0.04},
	}
	for _, c := range checks {
		if got := s.At(c.t); math.Abs(got-c.want) > 1e-6 {
			t.Errorf("error_ratio sine At(%v) = %v want %v", c.t, got, c.want)
		}
	}
}

// Integrated-emit tolerance follows the Python suite: per-step token count
// is floor(rate*dt) + Bernoulli(frac), so over N steps stddev ≈ sqrt(N*0.25);
// ±4σ bounds keep this from flaking without seeding.
func TestTokensForStepConstTotal(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	s, _ := ParseShape("const:5")
	count := 0
	for i := 0; i < 100; i++ {
		count += TokensForStep(rng, s.At(float64(i)/10), 0.1)
	}
	if count < 30 || count > 70 {
		t.Errorf("const:5 over 10s: %d outside [30,70]", count)
	}
}

func TestTokensForStepSineAverage(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	s, _ := ParseShape("sine:0,10,60")
	count := 0
	for i := 0; i < 600; i++ {
		count += TokensForStep(rng, s.At(float64(i)/10), 0.1)
	}
	// time-average 5 → expected 300, stddev ~12; ±40 wide.
	if count < 260 || count > 340 {
		t.Errorf("sine:0,10,60 over 60s: %d outside [260,340]", count)
	}
}

func ptr[T any](v T) *T { return &v }

func fpeq(a, b *float64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
