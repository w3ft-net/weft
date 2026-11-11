package coordinator

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/logtable"
)

// The coordinator sends compute the table the plan names, so a shard with a
// table per log source reads one table (docs/plans/per-service-tables.md).

func TestExecute_SendsTheTableTheSourceNames(t *testing.T) {
	c, log := keyedCoordinator(t, []string{"1", "2"}, []string{"3"}, nil, nil)

	got := runKeyed(t, c, `cat /log/vimgym/api/main.log`)
	if n := strings.Count(strings.TrimSpace(got), "\n") + 1; n != 3 {
		t.Fatalf("cat returned %d lines, want 3:\n%s", n, got)
	}

	log.mu.Lock()
	defer log.mu.Unlock()
	if len(log.reqs) == 0 {
		t.Fatal("no request reached compute")
	}
	want := logtable.Name([]any{int64(1)}, "vimgym", "api", "main")
	for _, r := range log.reqs {
		if r.Table != want {
			t.Errorf("request Table = %q, want %q", r.Table, want)
		}
	}
}

func TestExecute_NamesNoTableForMoreThanOneSource(t *testing.T) {
	c, log := keyedCoordinator(t, []string{"1"}, []string{"2"}, nil, nil)

	runKeyed(t, c, `cat /log/vimgym/api/main.log /log/vimgym/api/heartbeats.log`)

	log.mu.Lock()
	defer log.mu.Unlock()
	for _, r := range log.reqs {
		if r.Table != "" {
			t.Errorf("a two-source query sent Table %q, want none", r.Table)
		}
	}
}
