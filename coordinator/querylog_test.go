package coordinator

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// SetQueryLog must route audit lines to the dedicated file (creating a
// missing parent dir), and the line must keep the
// `2006/01/02 15:04:05 query: ...` shape weft's schema detection keys
// timestamps off. Regression guard for the cron self-reference fix:
// audit lines must not stay on the coordinator's main stderr stream.
func TestSetQueryLogRoutesToFileWithTimestamp(t *testing.T) {
	t.Cleanup(func() { queryLog.SetOutput(os.Stderr) })

	path := filepath.Join(t.TempDir(), "nested", "queries.log")
	if err := SetQueryLog(path); err != nil {
		t.Fatalf("SetQueryLog: %v", err)
	}

	queryLog.Printf("query: app=%s shards=%v", "weft/coordinator", []string{"s1"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	got := string(data)

	re := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} query: app=weft/coordinator shards=\[s1\]\n$`)
	if !re.MatchString(got) {
		t.Fatalf("audit line shape mismatch: %q", got)
	}
}

// Every query-audit line must carry coordinator_id once set, mirroring
// the field heartbeats/runtime-stats already carry (cmd/coordinator/
// main.go's HeartbeatWithFields/RuntimeStatsWithFields) — see
// docs/known-issues.md, "queries.log isn't tagged with coordinator_id".
// Without this, a query-routing imbalance across coordinator_desired=2
// instances is invisible from logs alone.
func TestQueryLogIncludesCoordinatorID(t *testing.T) {
	t.Cleanup(func() {
		queryLog.SetOutput(os.Stderr)
		SetQueryLogCoordinatorID("")
	})

	path := filepath.Join(t.TempDir(), "queries.log")
	if err := SetQueryLog(path); err != nil {
		t.Fatalf("SetQueryLog: %v", err)
	}
	SetQueryLogCoordinatorID("i-0b340ce3672315ed2")

	logQuery("query: namespace=%s service=%s shards=%v", "weft", "coordinator", []string{"s1"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	got := string(data)

	re := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} query: namespace=weft service=coordinator shards=\[s1\] coordinator_id=i-0b340ce3672315ed2\n$`)
	if !re.MatchString(got) {
		t.Fatalf("audit line shape mismatch: %q", got)
	}
}

// A failed redirect leaves the logger on stderr and surfaces the error
// rather than crashing — the coordinator must never fail to start over
// log routing.
func TestSetQueryLogErrorKeepsFallback(t *testing.T) {
	t.Cleanup(func() { queryLog.SetOutput(os.Stderr) })

	// A path whose parent is an existing regular file can't be created.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetQueryLog(filepath.Join(f, "queries.log")); err == nil {
		t.Fatal("expected error when parent path is a regular file")
	}
}
