package store

import (
	"fmt"
	"testing"
)

func TestValidateWatchPipeline(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
		wantErr  bool
	}{
		{"no alert", `cat /log/app.log | grep ERROR`, false},
		{"one alert", `check_errors | alert above 50`, false},
		{"one alert with notify", `check_errors | alert above 50 --notify=ops`, false},
		{"two alerts in pipeline", `check_errors | alert above 50 | alert below 10`, true},
		{"two alerts across lists", `alert above 1; alert below 2`, true},
		{"alert inside command sub", `echo $(check | alert above 1) | alert below 2`, true},
		{"alert inside if", `if true; then alert above 1; fi`, false},
		{"two alerts across if/for", `if true; then alert above 1; fi; for x in a; do alert below 2; done`, true},
		// Malformed shell syntax now surfaces as a parse error from
		// warp's tightened parser, so the validator rejects the
		// pipeline rather than silently letting it through. That's
		// the desired behaviour — a typo in a saved watch should be
		// caught at save time, not at run time.
		{"malformed pipeline is rejected", `<<< broken syntax >>>`, true},
		{"bad -k value", `foo | alert above 1 -k abc`, true},
		{"bad -t value", `foo | alert above 1 -k 2 -t 0`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateWatchPipeline(c.pipeline)
			if c.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", c.pipeline)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", c.pipeline, err)
			}
		})
	}
}

func TestCompileWatchPipeline(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
		wantKey  *string
		wantTag  *string
	}{
		{"no alert", `cat /log/app.log`, nil, nil},
		{"alert without -k/-t", `foo | alert above 1`, nil, nil},
		{"alert with -k only", `foo | alert above 1 -k 2`, strPtr("2"), nil},
		{"alert with -k and -t", `foo | alert above 1 -k 2 -t 1`, strPtr("2"), strPtr("1")},
		{"alert with --tag long form", `foo | alert above 1 -k 3 --tag 2`, strPtr("3"), strPtr("2")},
		{"alert with JSON-path -k", `foo | alert above 1 -k .latency_ms`, strPtr(".latency_ms"), nil},
		{"alert with JSON-path -k and -t", `foo | alert above 1 -k .v -t .host`, strPtr(".v"), strPtr(".host")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cw, err := CompileWatchPipeline(c.pipeline)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !eqStrPtr(cw.AlertKey, c.wantKey) {
				t.Errorf("AlertKey = %v, want %v", derefStr(cw.AlertKey), derefStr(c.wantKey))
			}
			if !eqStrPtr(cw.AlertTag, c.wantTag) {
				t.Errorf("AlertTag = %v, want %v", derefStr(cw.AlertTag), derefStr(c.wantTag))
			}
		})
	}
}

// TestCompileWatchPipeline_AlertThreshold confirms the dashboard
// projection (AlertCondition + AlertThreshold) is populated for
// numeric-cutoff conditions and stays nil for everything else. The
// Sparkline draws a line only when both fields are non-nil, so this
// also guards against accidentally populating one without the other.
func TestCompileWatchPipeline_AlertThreshold(t *testing.T) {
	cases := []struct {
		name      string
		pipeline  string
		wantCond  *string
		wantThres *float64
	}{
		{"no alert", `cat /log/app.log`, nil, nil},
		{"above", `foo | alert above 50`, strPtr("above"), float64Ptr(50)},
		{"below", `foo | alert below 10`, strPtr("below"), float64Ptr(10)},
		{"eq", `foo | alert eq 0`, strPtr("eq"), float64Ptr(0)},
		{"neq", `foo | alert neq 42`, strPtr("neq"), float64Ptr(42)},
		// rising/falling use a percent delta (PctChange), not an
		// absolute threshold. Leave both nil so no line renders.
		{"rising", `foo | alert rising 20%`, nil, nil},
		{"falling", `foo | alert falling 10%`, nil, nil},
		// No numeric dimension at all.
		{"changed", `foo | alert changed`, nil, nil},
		{"absent", `foo | alert absent`, nil, nil},
		{"any (no keyword)", `foo | alert --notify=ops`, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cw, err := CompileWatchPipeline(c.pipeline)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !eqStrPtr(cw.AlertCondition, c.wantCond) {
				t.Errorf("AlertCondition = %v, want %v", derefStr(cw.AlertCondition), derefStr(c.wantCond))
			}
			if !eqFloat64Ptr(cw.AlertThreshold, c.wantThres) {
				t.Errorf("AlertThreshold = %v, want %v", derefF64(cw.AlertThreshold), derefF64(c.wantThres))
			}
		})
	}
}

func intPtr(n int) *int             { return &n }
func strPtr(s string) *string       { return &s }
func float64Ptr(v float64) *float64 { return &v }

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqStrPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func eqFloat64Ptr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func derefStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func derefF64(p *float64) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%v", *p)
}
