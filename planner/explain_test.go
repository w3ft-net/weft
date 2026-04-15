//go:build integration

package planner

import (
	"strings"
	"testing"

	"github.com/kevlarcon/warp/pkg/bash"
	"github.com/kevlarcon/warp/pkg/jq"
)

func TestExplain_JqPctExtraction(t *testing.T) {
	// Simulate a metrics row as ingested by weftd: Raw and PayloadJson are
	// identical JSON. After CAST(payload AS TEXT), raw should still be valid
	// JSON that jq can consume.
	metricsJSON := `{"type":"memory","total_mb":7864,"used_mb":4521,"available_mb":3343,"swap_used_mb":0,"pct":57.49}`

	t.Run("jq.Run extracts .pct from raw JSON", func(t *testing.T) {
		out, err := jq.Run(".pct", metricsJSON)
		if err != nil {
			t.Fatalf("jq.Run: %v", err)
		}
		if out != "57.49" {
			t.Errorf("got %q, want %q", out, "57.49")
		}
	})

	t.Run("bash pipeline extracts .pct via jq", func(t *testing.T) {
		input := metricsJSON + "\n"
		stdout, stderr, exitCode, err := bash.Run("jq '.pct'", input)
		if err != nil {
			t.Fatalf("bash.Run: %v", err)
		}
		if exitCode != 0 {
			t.Fatalf("exit %d: %s", exitCode, stderr)
		}
		got := strings.TrimSpace(stdout)
		if got != "57.49" {
			t.Errorf("got %q, want %q", got, "57.49")
		}
	})

	t.Run("multi-line JSON through jq pipeline", func(t *testing.T) {
		lines := []string{
			`{"type":"memory","pct":57.49}`,
			`{"type":"memory","pct":62.1}`,
			`{"type":"memory","pct":45}`,
		}
		input := strings.Join(lines, "\n") + "\n"
		stdout, stderr, exitCode, err := bash.Run("jq '.pct'", input)
		if err != nil {
			t.Fatalf("bash.Run: %v", err)
		}
		if exitCode != 0 {
			t.Fatalf("exit %d: %s", exitCode, stderr)
		}
		got := strings.TrimSpace(stdout)
		want := "57.49\n62.1\n45"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("double-quoted JSON fails jq parse", func(t *testing.T) {
		// If CAST(JSON AS TEXT) were to produce a quoted string instead of
		// bare JSON, jq would see a string, not an object.
		doubleQuoted := `"{\"type\":\"memory\",\"pct\":57.49}"`
		_, err := jq.Run(".pct", doubleQuoted)
		if err == nil {
			t.Error("expected error indexing a string with .pct, got nil")
		}
	})
}

func TestExplain_PlannerOutput(t *testing.T) {
	t.Run("jq select with field access splits correctly", func(t *testing.T) {
		plan, err := Plan(`tail log/weft/metrics.log | jq 'select(.type == "memory") | .pct'`)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}

		if plan.SourceCmd != "tail" {
			t.Errorf("SourceCmd = %q, want %q", plan.SourceCmd, "tail")
		}
		if plan.SourceApp != "weft/metrics" {
			t.Errorf("SourceApp = %q, want %q", plan.SourceApp, "weft/metrics")
		}

		wantSQL := `SELECT * FROM logs WHERE app = 'weft/metrics' AND json_extract_string(payload, '$.type') = 'memory' ORDER BY ts DESC LIMIT 10`
		if plan.ShardSQL != wantSQL {
			t.Errorf("ShardSQL\n got: %s\nwant: %s", plan.ShardSQL, wantSQL)
		}

		if len(plan.ComputeStages) != 1 || plan.ComputeStages[0] != "jq '.pct'" {
			t.Errorf("ComputeStages = %v, want [jq '.pct']", plan.ComputeStages)
		}

		if plan.Project == nil {
			t.Fatal("Project is nil, expected json_keys projection")
		}
		if len(plan.Project.JSONKeys) != 1 || plan.Project.JSONKeys[0] != "pct" {
			t.Errorf("Project.JSONKeys = %v, want [pct]", plan.Project.JSONKeys)
		}
	})

	t.Run("explain output includes all sections", func(t *testing.T) {
		output := Explain(`tail log/weft/metrics.log | jq 'select(.type == "memory") | .pct'`)

		for _, want := range []string{
			"Source:",
			"command:  tail",
			"app:      weft/metrics",
			"Shard SQL:",
			"json_extract_string(payload, '$.type') = 'memory'",
			"Compute stages:",
			"jq '.pct'",
			"Projection:",
			"json_keys",
			"Final SQL (after projection):",
			"CAST(payload AS TEXT) as raw",
		} {
			if !strings.Contains(output, want) {
				t.Errorf("explain output missing %q\n\nfull output:\n%s", want, output)
			}
		}
	})
}
