package planner

import (
	"strings"
	"testing"
	"time"
)

func TestPlan_SourceRejectsUnknownFlags(t *testing.T) {
	for _, c := range []struct{ pipeline, flag string }{
		{`tail -c 3 /log/acme/prod/api/main.log`, "-c"},
		{`tail -F /log/acme/prod/api/main.log`, "-F"},
		{`head -c 3 /log/acme/prod/api/main.log`, "-c"},
		{`head -q /log/acme/prod/api/main.log`, "-q"},
	} {
		_, err := planBare(c.pipeline, "", time.Time{}, time.Time{})
		want := " unknown option " + c.flag
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Plan(%q) = %v, want an error containing %q", c.pipeline, err, want)
		}
	}
}

func TestPlan_SourceKeepsSupportedFlags(t *testing.T) {
	for _, p := range []string{
		`tail -f /log/acme/prod/api/main.log`,
		`tail -n 5 /log/acme/prod/api/main.log`,
		`tail -5 /log/acme/prod/api/main.log`,
		`head -n 5 /log/acme/prod/api/main.log`,
		`head -5 /log/acme/prod/api/main.log`,
	} {
		if _, err := planBare(p, "", time.Time{}, time.Time{}); err != nil {
			t.Errorf("Plan(%q) = %v", p, err)
		}
	}
}
