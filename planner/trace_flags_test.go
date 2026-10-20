package planner

import (
	"strings"
	"testing"
	"time"
)

func TestPlan_TraceRejectsUnknownOptions(t *testing.T) {
	for _, c := range []struct{ pipeline, want string }{
		{`trace --bogus`, "trace: unknown option --bogus"},
		{`trace abc123 --bogus`, "trace: unknown option --bogus"},
		{`trace -v abc123`, "trace: unknown option -v"},
		{`trace spans --bogus`, "trace spans: unknown option --bogus"},
		{`trace spans --by host`, "trace spans: unknown option --by"},
	} {
		_, err := Plan(c.pipeline, 0, "", time.Time{}, time.Time{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Plan(%q) = %v, want an error containing %q", c.pipeline, err, c.want)
		}
	}
}

func TestPlan_TraceKeepsSupportedForms(t *testing.T) {
	for _, p := range []string{
		`trace abc123`,
		`trace abc123 --spans`,
		`trace --spans abc123`,
		`trace spans`,
		`trace spans db.query`,
		`trace spans --by service`,
		`trace spans db.query --samples`,
	} {
		if _, err := Plan(p, 0, "", time.Time{}, time.Time{}); err != nil {
			t.Errorf("Plan(%q) = %v", p, err)
		}
	}
}
