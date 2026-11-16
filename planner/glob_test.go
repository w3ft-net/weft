package planner

import (
	"strings"
	"testing"
	"time"
)

// Globs match kinds inside one service directory, nothing wider.
// Apps come from coordinator.SeenServices as "<ns>/<env>/<svc>/<kind>"
// (env-less: "<ns>/<svc>/<kind>"); see encodeName.
var globApps = []string{
	"acme/prod/api/main",
	"acme/prod/api/metrics",
	"acme/prod/edge/main",
	"acme/sand/api/main",
	"weft/shard/main",
}

func TestExpandSourceGlobs(t *testing.T) {
	tests := []struct {
		name     string
		pipeline string
		cwd      string
		want     string
	}{
		{
			name:     "no glob passes through",
			pipeline: `cat /log/acme/prod/api/main.log | wc -l`,
			want:     `cat /log/acme/prod/api/main.log | wc -l`,
		},
		{
			name:     "star expands to the service's kinds",
			pipeline: `cat /log/acme/prod/api/*.log`,
			want:     `cat /log/acme/prod/api/main.log /log/acme/prod/api/metrics.log`,
		},
		{
			name:     "prefix glob filters kinds",
			pipeline: `tail -n 5 /log/acme/prod/api/me*.log | grep x`,
			want:     `tail -n 5 /log/acme/prod/api/metrics.log | grep x`,
		},
		{
			name:     "single ? wildcard, env-less service",
			pipeline: `head -5 /log/weft/shard/?ain.log`,
			want:     `head -5 /log/weft/shard/main.log`,
		},
		{
			name:     "relative glob inside a service directory",
			pipeline: `cat *.log | grep x`,
			cwd:      `/log/acme/prod/api`,
			want:     `cat /log/acme/prod/api/main.log /log/acme/prod/api/metrics.log | grep x`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandSourceGlobs(tt.pipeline, tt.cwd, globApps)
			if err != nil {
				t.Fatalf("ExpandSourceGlobs: %v", err)
			}
			if got != tt.want {
				t.Errorf("\n got:  %s\nwant: %s", got, tt.want)
			}
		})
	}
}

// A glob that would read across services or namespaces is refused with
// an error that says so and names the alternatives, never expanded and
// never reported as a vague "no match".
func TestExpandSourceGlobs_CrossSourceIsDisallowed(t *testing.T) {
	tests := []struct{ name, pipeline, cwd string }{
		{"bare from home", `cat *.log | grep x`, ""},
		{"bare from slash", `cat *.log | grep x`, "/"},
		{"bare from a namespace dir", `cat *.log | grep x`, "/log/acme"},
		{"bare from an env dir", `cat *.log | grep x`, "/log/acme/prod"},
		{"absolute env dir", `cat /log/acme/prod/*.log`, ""},
		{"absolute namespace dir", `tail /log/acme/*.log`, ""},
		{"wildcard service segment", `cat /log/acme/prod/*/main.log`, ""},
		{"wildcard env segment", `cat /log/acme/*/api/main.log`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ExpandSourceGlobs(tt.pipeline, tt.cwd, globApps)
			if err == nil {
				t.Fatal("expected the cross-source glob to be refused")
			}
			for _, want := range []string{"not allowed", "across", "<ns>.log"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %q: %v", want, err)
				}
			}
		})
	}
}

func TestExpandSourceGlobs_NoMatchInServiceIsAnError(t *testing.T) {
	_, err := ExpandSourceGlobs(`cat /log/acme/prod/api/*_audit.log`, "", globApps)
	if err == nil {
		t.Fatal("expected error when glob matches nothing")
	}
	if !strings.Contains(err.Error(), "no pseudofiles match") {
		t.Errorf("error message should mention 'no pseudofiles match': %v", err)
	}
}

// A glob that reaches the planner unexpanded must be refused, not
// planned: `*` would become the literal kind '*' and match nothing.
func TestPlan_UnexpandedGlobIsRefused(t *testing.T) {
	for _, p := range []string{
		`tail -n 1 /log/acme/prod/api/*.log`,
		`cat /log/acme/prod/api/m?in.log`,
	} {
		_, err := planBare(p, "", time.Time{}, time.Time{})
		if err == nil || !strings.Contains(err.Error(), "glob") {
			t.Errorf("Plan(%q) = %v, want a glob error", p, err)
		}
	}
}
