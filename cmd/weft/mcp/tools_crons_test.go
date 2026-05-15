package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// TestCronsList_HappyPath: names come from ListWatches, each cron's
// schedule+pipeline from GetCron, rendered as one block per cron in
// name-sorted order. This is the tool that lets the model recover a
// cron's exact spec before proposing a corrected version.
func TestCronsList_HappyPath(t *testing.T) {
	bodies := map[string]string{
		"stream-errors":       "schedule: */5 * * * *\npipeline: tail -n 5000 /log/weft/coordinator/main.log | grep -c 'stream error' | alert above 0\n",
		"compute-exit-127":    "schedule: */5 * * * *\npipeline: tail -n 500 /log/weft/coordinator/main.log | grep -c 'compute exit 127' | alert above 0\n",
		"shards-without-node": "schedule: */5 * * * *\npipeline: tail -n 1000 /log/weft/coordinator/main.log | grep -c 'no LAUNCHING row' | alert above 0\n",
	}
	s := testSession(t, Config{
		Run: neverRunFn(t),
		ListWatches: func() ([]string, error) {
			return []string{"stream-errors", "compute-exit-127", "shards-without-node"}, nil
		},
		GetCron: func(name string) (string, error) {
			b, ok := bodies[name]
			if !ok {
				t.Errorf("GetCron called with unknown name %q", name)
			}
			return b, nil
		},
	})

	res := callTool(t, s, "weft.crons_list", map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected IsError; content=%q", textContent(res))
	}
	got := textContent(res)

	// Name-sorted: compute-exit-127 before shards-without-node before
	// stream-errors.
	if i, j := strings.Index(got, "compute-exit-127"), strings.Index(got, "stream-errors"); i < 0 || j < 0 || i > j {
		t.Errorf("crons not in sorted order:\n%s", got)
	}
	for _, want := range []string{
		"name: shards-without-node",
		"schedule: */5 * * * *",
		"grep -c 'no LAUNCHING row' | alert above 0",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}
}

// TestCronsList_NotConfigured: with the hooks unset the tool must
// report a clean tool-error, never panic on a nil call.
func TestCronsList_NotConfigured(t *testing.T) {
	s := testSession(t, Config{Run: neverRunFn(t)})

	res := callTool(t, s, "weft.crons_list", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected IsError when ListWatches/GetCron unset; got %q", textContent(res))
	}
	if !strings.Contains(textContent(res), "not configured") {
		t.Errorf("unhelpful error: %q", textContent(res))
	}
}

// TestCronsList_Empty: no crons is a valid state, not an error.
func TestCronsList_Empty(t *testing.T) {
	s := testSession(t, Config{
		Run:         neverRunFn(t),
		ListWatches: func() ([]string, error) { return nil, nil },
		GetCron:     func(string) (string, error) { return "", nil },
	})

	res := callTool(t, s, "weft.crons_list", map[string]any{})
	if res.IsError {
		t.Fatalf("empty cron list should not be an error; got %q", textContent(res))
	}
	if !strings.Contains(textContent(res), "no crons defined") {
		t.Errorf("expected 'no crons defined', got %q", textContent(res))
	}
}

// TestCronsList_ListError surfaces a backend list failure as a tool
// error rather than an empty success.
func TestCronsList_ListError(t *testing.T) {
	s := testSession(t, Config{
		Run:         neverRunFn(t),
		ListWatches: func() ([]string, error) { return nil, fmt.Errorf("backend down") },
		GetCron:     func(string) (string, error) { return "", nil },
	})

	res := callTool(t, s, "weft.crons_list", map[string]any{})
	if !res.IsError {
		t.Fatalf("expected IsError on list failure; got %q", textContent(res))
	}
	if !strings.Contains(textContent(res), "backend down") {
		t.Errorf("error should surface the cause; got %q", textContent(res))
	}
}
