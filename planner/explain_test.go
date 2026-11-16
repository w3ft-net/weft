//go:build integration

package planner

import (
	"bytes"
	"strings"
	"testing"

	"time"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/jq"
)

func runJq(expr, input string) (string, error) {
	var out bytes.Buffer
	if err := jq.Run(expr, strings.NewReader(input), &out); err != nil {
		return "", err
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

func TestExplain_JqPctExtraction(t *testing.T) {
	metricsJSON := `{"type":"memory","total_mb":7864,"used_mb":4521,"available_mb":3343,"swap_used_mb":0,"pct":57.49}`

	t.Run("jq.Run extracts .pct from raw JSON", func(t *testing.T) {
		out, err := runJq(".pct", metricsJSON)
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
		doubleQuoted := `"{\"type\":\"memory\",\"pct\":57.49}"`
		_, err := runJq(".pct", doubleQuoted)
		if err == nil {
			t.Error("expected error indexing a string with .pct, got nil")
		}
	})
}

func TestExplain_PlannerOutput(t *testing.T) {
	t.Run("jq select with field access splits correctly", func(t *testing.T) {
		plan, err := planBare(`tail /log/weft/metrics/main.log | jq 'select(.type == "memory") | .pct'`, "", time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}

		if plan.SourceCmd != "tail" {
			t.Errorf("SourceCmd = %q, want %q", plan.SourceCmd, "tail")
		}
		if plan.SourceNamespace != "weft" || plan.SourceService != "metrics" {
			t.Errorf("source = %q/%q, want weft/metrics", plan.SourceNamespace, plan.SourceService)
		}

		// tail's line count ends predicate pushdown, so jq stays in compute and
		// the shard narrows each payload to the keys it reads.
		if !strings.Contains(plan.ShardSQL, "json_object('pct', json_extract(payload, '$.pct'), 'type', json_extract(payload, '$.type'))") {
			t.Errorf("ShardSQL does not narrow the payload to .pct and .type\n got: %s", plan.ShardSQL)
		}
		if len(plan.ComputeStages) != 1 || !strings.HasPrefix(plan.ComputeStages[0], "jq ") {
			t.Errorf("ComputeStages = %v, want the one jq stage", plan.ComputeStages)
		}
	})

	t.Run("explain output includes all sections", func(t *testing.T) {
		output := Explain(`tail /log/weft/metrics/main.log | jq 'select(.type == "memory") | .pct'`)

		for _, want := range []string{
			"Scan     namespace=weft service=metrics kind=main",
			"Limit    10  ts DESC",
			"Filter   exact",
			"Passes:",
			"field-projection     Filter[1: jq] reads only .pct, .type",
			"Shard SQL:",
			"json_object('pct', json_extract(payload, '$.pct'), 'type', json_extract(payload, '$.type'))",
		} {
			if !strings.Contains(output, want) {
				t.Errorf("explain output missing %q\n\nfull output:\n%s", want, output)
			}
		}
	})
}
