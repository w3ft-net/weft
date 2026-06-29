package planner

import (
	"strings"
	"testing"
)

func TestExpandSourceGlobs(t *testing.T) {
	// Apps come from coordinator.SeenServices, where env-bearing entries
	// are encoded as "<ns>/<env>/<deployment>" (env-less ones stay
	// "<ns>/<deployment>") — see encodeName in
	// coordinator/coordinator.go. Both shapes appear in the same list.
	apps := []string{
		"acme/prod/api",
		"acme/prod/api_metrics",
		"acme/prod/edge",
		"acme/prod/edge_metrics",
		"acme/prod/worker",
		"acme/prod/worker_metrics",
		"acme/sand/api",
		"acme/sand/edge",
		"weft/coordinator",
		"weft/shard",
	}
	tests := []struct {
		name     string
		pipeline string
		want     string
	}{
		{
			name:     "no glob passes through",
			pipeline: `cat /log/acme/prod/api.log | wc -l`,
			want:     `cat /log/acme/prod/api.log | wc -l`,
		},
		{
			name:     "star expands within env subdir",
			pipeline: `cat /log/acme/prod/*.log`,
			want:     `cat /log/acme/prod/api.log /log/acme/prod/api_metrics.log /log/acme/prod/edge.log /log/acme/prod/edge_metrics.log /log/acme/prod/worker.log /log/acme/prod/worker_metrics.log`,
		},
		{
			name:     "suffix glob filters by name",
			pipeline: `cat /log/acme/prod/*_metrics.log`,
			want:     `cat /log/acme/prod/api_metrics.log /log/acme/prod/edge_metrics.log /log/acme/prod/worker_metrics.log`,
		},
		{
			name:     "single ? wildcard",
			pipeline: `cat /log/weft/?hard.log`,
			want:     `cat /log/weft/shard.log`,
		},
		{
			name:     "preserves downstream stages",
			pipeline: `cat /log/acme/prod/*.log | grep ERROR | wc -l`,
			want:     `cat /log/acme/prod/api.log /log/acme/prod/api_metrics.log /log/acme/prod/edge.log /log/acme/prod/edge_metrics.log /log/acme/prod/worker.log /log/acme/prod/worker_metrics.log | grep ERROR | wc -l`,
		},
		{
			name:     "head respects globs too",
			pipeline: `head -5 /log/acme/prod/*_metrics.log`,
			want:     `head -5 /log/acme/prod/api_metrics.log /log/acme/prod/edge_metrics.log /log/acme/prod/worker_metrics.log`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExpandSourceGlobs(tt.pipeline, "", apps)
			if err != nil {
				t.Fatalf("ExpandSourceGlobs: %v", err)
			}
			if got != tt.want {
				t.Errorf("\n got:  %s\nwant: %s", got, tt.want)
			}
		})
	}
}

func TestExpandSourceGlobs_NoMatchIsAnError(t *testing.T) {
	apps := []string{"acme/prod/api"}
	_, err := ExpandSourceGlobs(`cat /log/acme/prod/*_metrics.log`, "", apps)
	if err == nil {
		t.Fatal("expected error when glob matches nothing")
	}
	if !strings.Contains(err.Error(), "no pseudofiles match") {
		t.Errorf("error message should mention 'no pseudofiles match': %v", err)
	}
}
