package store

import "testing"

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
