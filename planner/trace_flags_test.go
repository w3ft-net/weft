package planner

import (
	"strings"
	"testing"
	"time"
)

func TestPlan_TraceRejectsUnknownOptions(t *testing.T) {
	for _, c := range []struct{ pipeline, want string }{
		{`trace --bogus`, "trace: unknown option --bogus"},
		{`trace --id abc123 --bogus`, "trace: unknown option --bogus"},
		{`trace -v`, "trace: unknown option -v"},
		{`trace --spans --bogus`, "trace: unknown option --bogus"},
		{`trace --spans --by host`, "trace: unknown option --by"},
		{`trace --spans --by service`, "trace: unknown option --by"},
	} {
		_, err := Plan(c.pipeline, 0, "", time.Time{}, time.Time{})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Plan(%q) = %v, want an error containing %q", c.pipeline, err, c.want)
		}
	}
}

func TestPlan_TraceKeepsSupportedForms(t *testing.T) {
	for _, p := range []string{
		`trace`,
		`trace acme`,
		`trace acme/api`,
		`trace --id abc123`,
		`trace --id abc123 acme/api`,
		`trace --spans --id abc123`,
		`trace --spans`,
		`trace --spans acme/api`,
		`trace --spans --name db.query`,
		`trace --spans --name db.query --samples`,
	} {
		if _, err := Plan(p, 0, "", time.Time{}, time.Time{}); err != nil {
			t.Errorf("Plan(%q) = %v", p, err)
		}
	}
}
