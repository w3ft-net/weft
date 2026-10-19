package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	weftsdk "github.com/w3ft-net/weft-go"
)

// auditLine decodes the one JSON line an audit write produced.
func auditLine(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var rec map[string]any
	if err := json.Unmarshal(data, &rec); err != nil || bytes.Count(data, []byte("\n")) != 1 {
		t.Fatalf("want one JSON audit line, got %q (%v)", data, err)
	}
	return rec
}

// SetQueryLog must route audit lines to the dedicated file (creating a
// missing parent dir). Regression guard for the cron self-reference fix:
// audit lines must not stay on the coordinator's main stderr stream.
func TestSetQueryLogRoutesToFileWithTimestamp(t *testing.T) {
	t.Cleanup(func() { queryOut.Set(os.Stderr) })

	path := filepath.Join(t.TempDir(), "nested", "queries.log")
	if err := SetQueryLog(path); err != nil {
		t.Fatalf("SetQueryLog: %v", err)
	}

	logQuery(context.Background(), "query: app=%s shards=%v", "weft/coordinator", []string{"s1"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if rec := auditLine(t, data); rec["msg"] != "query: app=weft/coordinator shards=[s1]" || rec["time"] == nil {
		t.Fatalf("audit line = %v", rec)
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
		queryOut.Set(os.Stderr)
		SetQueryLogCoordinatorID("")
	})

	path := filepath.Join(t.TempDir(), "queries.log")
	if err := SetQueryLog(path); err != nil {
		t.Fatalf("SetQueryLog: %v", err)
	}
	SetQueryLogCoordinatorID("i-0b340ce3672315ed2")

	logQuery(context.Background(), "query: namespace=%s service=%s shards=%v", "weft", "coordinator", []string{"s1"})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	rec := auditLine(t, data)
	if rec["msg"] != "query: namespace=weft service=coordinator shards=[s1]" || rec["coordinator_id"] != "i-0b340ce3672315ed2" {
		t.Fatalf("audit line = %v", rec)
	}
}

// A failed redirect leaves the logger on stderr and surfaces the error
// rather than crashing — the coordinator must never fail to start over
// log routing.
func TestSetQueryLogErrorKeepsFallback(t *testing.T) {
	t.Cleanup(func() { queryOut.Set(os.Stderr) })

	// A path whose parent is an existing regular file can't be created.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SetQueryLog(filepath.Join(f, "queries.log")); err == nil {
		t.Fatal("expected error when parent path is a regular file")
	}
}

// An audit line for a traced request carries its trace ID, after
// coordinator_id, without the call site spelling it.
func TestQueryLogCarriesTheTraceID(t *testing.T) {
	t.Cleanup(func() {
		queryOut.Set(os.Stderr)
		SetQueryLogCoordinatorID("")
	})
	path := filepath.Join(t.TempDir(), "queries.log")
	if err := SetQueryLog(path); err != nil {
		t.Fatal(err)
	}
	SetQueryLogCoordinatorID("i-1")

	ctx := weftsdk.ContextWithTraceID(context.Background(), "7f778536850bbbbaed3c25469f1a23dd")
	logQuery(ctx, "query: namespace=%s", "weft")

	data, _ := os.ReadFile(path)
	rec := auditLine(t, data)
	if rec["msg"] != "query: namespace=weft" || rec["coordinator_id"] != "i-1" || rec["trace_id"] != "7f778536850bbbbaed3c25469f1a23dd" {
		t.Fatalf("audit line = %v", rec)
	}
}

// With SDK logging the audit stream rides the main file as kind=queries.
func TestLogQuery_SDKFileUsesQueriesKind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coordinator.log")
	t.Setenv(weftsdk.EnvLogFile, path)
	prev := slog.Default()
	defer slog.SetDefault(prev)
	lf := weftsdk.InitLogging("", io.Discard)
	if lf == nil {
		t.Fatal("no log file")
	}
	defer lf.Close()
	SetQueryLogFile(lf)
	defer SetQueryLogFile(nil)

	logQuery(context.Background(), "query: select 1")
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "\x1fqueries\x1e") || !strings.Contains(string(data), "query: select 1") {
		t.Errorf("audit line = %q, want kind queries", data)
	}
}
