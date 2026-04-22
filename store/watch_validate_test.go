package store

import (
	"testing"
)

func TestValidateWatchPipeline(t *testing.T) {
	cases := []struct {
		name     string
		pipeline string
		wantErr  bool
	}{
		{"no alert", `cat /log/app.log --since 5m | grep ERROR`, false},
		{"one alert", `check_errors | alert above 50`, false},
		{"one alert with notify", `check_errors | alert above 50 --notify=ops`, false},
		{"two alerts in pipeline", `check_errors | alert above 50 | alert below 10`, true},
		{"two alerts across lists", `alert above 1; alert below 2`, true},
		{"alert inside command sub", `echo $(check | alert above 1) | alert below 2`, true},
		{"alert inside if", `if true; then alert above 1; fi`, false},
		{"two alerts across if/for", `if true; then alert above 1; fi; for x in a; do alert below 2; done`, true},
		{"unparseable pipeline is passed through", `<<< broken syntax >>>`, false},
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
		wantKey  *int
		wantTag  *int
	}{
		{"no alert", `cat /log/app.log`, nil, nil},
		{"alert without -k/-t", `foo | alert above 1`, nil, nil},
		{"alert with -k only", `foo | alert above 1 -k 2`, intPtr(2), nil},
		{"alert with -k and -t", `foo | alert above 1 -k 2 -t 1`, intPtr(2), intPtr(1)},
		{"alert with --tag long form", `foo | alert above 1 -k 3 --tag 2`, intPtr(3), intPtr(2)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cw, err := CompileWatchPipeline(c.pipeline)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !eqIntPtr(cw.AlertKey, c.wantKey) {
				t.Errorf("AlertKey = %v, want %v", cw.AlertKey, c.wantKey)
			}
			if !eqIntPtr(cw.AlertTag, c.wantTag) {
				t.Errorf("AlertTag = %v, want %v", cw.AlertTag, c.wantTag)
			}
		})
	}
}

func intPtr(n int) *int { return &n }

func eqIntPtr(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
