package coordinator

import (
	"strings"
	"testing"
)

// setupProjectCoordinator creates a coordinator with compute pool for
// projection tests.
func setupProjectCoordinator(t *testing.T) *Coordinator {
	t.Helper()
	return setupTestCoordinator(t)
}

// ---------------------------------------------------------------------------
// Field projection tests (cut)
// ---------------------------------------------------------------------------

func TestProjectCoord_CutSingleField(t *testing.T) {
	c := setupProjectCoordinator(t)
	// cut -d' ' -f3 should return only the 3rd field of each line (the app name).
	res, err := c.Execute(`cat vimgym.log | cut -d' ' -f3`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	// With a complete projection, result should be text (cut is handled as
	// projection + the shard reconstructs raw from selected fields).
	// Each line should be just the app name "vimgym".
	output := resultText(t, res)
	lines := nonEmptyLines(output)
	if len(lines) == 0 {
		t.Fatal("expected output lines")
	}
	for i, l := range lines {
		if l != "vimgym" {
			t.Errorf("line %d: expected 'vimgym', got %q", i, l)
		}
	}
}

func TestProjectCoord_CutMultipleFields(t *testing.T) {
	c := setupProjectCoordinator(t)
	// cut -d' ' -f1,2 should return timestamp and level.
	res, err := c.Execute(`cat vimgym.log | grep ERROR | cut -d' ' -f1,2`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	output := resultText(t, res)
	lines := nonEmptyLines(output)
	// Each line should be "TIMESTAMP LEVEL" — two fields only.
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) != 2 {
			t.Errorf("expected 2 fields, got %d: %q", len(fields), l)
		}
		if fields[1] != "ERROR" {
			t.Errorf("expected level ERROR, got %q", fields[1])
		}
	}
	// Should have 5 ERROR lines from vimgym.
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
	}
}

func TestProjectCoord_CutOpenEnded(t *testing.T) {
	c := setupProjectCoordinator(t)
	// cut -d' ' -f5- should return everything from field 5 onward (the message).
	res, err := c.Execute(`cat vimgym.log | grep ERROR | cut -d' ' -f5-`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	output := resultText(t, res)
	lines := nonEmptyLines(output)
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
	}
	// Lines should contain the message portion, not the timestamp/level/app/instance.
	for _, l := range lines {
		if strings.HasPrefix(l, "2026") {
			t.Errorf("line should not start with timestamp: %q", l)
		}
	}
}

func TestProjectCoord_CutThenSort(t *testing.T) {
	c := setupProjectCoordinator(t)
	// cut then sort — projection narrows, sort runs in compute on narrowed data.
	res, err := c.Execute(`cat vimgym.log | cut -d' ' -f2 | sort | uniq -c`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	output := resultText(t, res)
	lines := nonEmptyLines(output)
	// Should have counts of each log level for vimgym.
	if len(lines) == 0 {
		t.Fatal("expected output")
	}
	// Each line should be a count + level.
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		fields := strings.Fields(trimmed)
		if len(fields) != 2 {
			t.Errorf("expected 'COUNT LEVEL', got %q", l)
		}
	}
}

// ---------------------------------------------------------------------------
// awk projection tests
// ---------------------------------------------------------------------------

func TestProjectCoord_AwkPrintField(t *testing.T) {
	c := setupProjectCoordinator(t)
	// awk print $3 — projection on field 3, compute still runs awk for formatting.
	res, err := c.Execute(`cat vimgym.log | awk '{ print $3 }'`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	output := resultText(t, res)
	lines := nonEmptyLines(output)
	// All vimgym lines: field 3 is the app name "vimgym".
	for _, l := range lines {
		if l != "vimgym" {
			t.Errorf("expected 'vimgym', got %q", l)
		}
	}
}

// ---------------------------------------------------------------------------
// jq projection tests
// ---------------------------------------------------------------------------

func TestProjectCoord_JqObjectConstruction(t *testing.T) {
	c := setupProjectCoordinator(t)
	// jq object construction should work and only needs specific JSON keys.
	res, err := c.Execute(`cat vimgym.log | awk '$LEVEL == "ERROR"' | jq '{action: .action}'`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	output := resultText(t, res)
	lines := nonEmptyLines(output)
	if len(lines) == 0 {
		t.Fatal("expected output")
	}
	// Each line should be a JSON object with just the "action" key.
	for _, l := range lines {
		if !strings.Contains(l, "action") {
			t.Errorf("expected 'action' in output, got %q", l)
		}
	}
}

// ---------------------------------------------------------------------------
// Verify projection doesn't break non-projection queries
// ---------------------------------------------------------------------------

func TestProjectCoord_NoProjectionRegression(t *testing.T) {
	c := setupProjectCoordinator(t)

	// Pure filter — no projection.
	res, err := c.Execute(`cat vimgym.log | awk '$LEVEL == "ERROR"'`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if len(resD.Rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(resD.Rows))
	}

	// grep + wc — compute only, no projection.
	res2, err := c.Execute(`cat warp.log | grep timeout | wc -l`, fullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	res2D := drainExec(t, res2)
	if strings.TrimSpace(res2D.Text) != "5" {
		t.Errorf("expected 5, got %q", res2D.Text)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resultText returns the text output from an ExecuteResult, preferring .Text
// over formatted .Rows. Drains the stream (no-op for already-buffered
// results) so callers don't need to.
func resultText(t *testing.T, res *ExecuteResult) string {
	t.Helper()
	resD := drainExec(t, res)
	if resD.Text != "" {
		return resD.Text
	}
	var sb strings.Builder
	for _, r := range resD.Rows {
		sb.WriteString(r.Raw)
		sb.WriteByte('\n')
	}
	return sb.String()
}
