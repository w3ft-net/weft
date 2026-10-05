package main

import (
	"context"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/coordinator/service"
	"github.com/graybeardcorp/weft/store"
)

// capture runs fn and returns what it wrote to stdout.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	fn()
	w.Close()
	os.Stdout = old
	return <-done
}

// The whole chain: the cron-mode command -> admin gRPC (token-gated) ->
// handlers -> Postgres, through a dry run, set, list and rollback.
func TestCronModeCommandEndToEnd(t *testing.T) {
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	tx, rollback := db.BeginTest()
	t.Cleanup(func() { rollback(); db.Close() })

	org, err := tx.FindOrCreateOrg("cron-mode-e2e")
	if err != nil {
		t.Fatal(err)
	}
	w := store.CronWatch{OrgID: org.ID, Name: "e2e-cron", Schedule: "every 5m", Pipeline: "true", Enabled: true}
	if err := tx.DB().WithContext(store.WithoutTenantScope(context.Background())).Create(&w).Error; err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatUint(uint64(w.ID), 10)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := service.NewAdminGRPCServer(&service.Server{Store: tx}, "e2e-token")
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)
	t.Setenv(adminTokenEnv, "e2e-token")
	addr := lis.Addr().String()

	run := func(args ...string) string {
		return capture(t, func() { cronModeCmd(append(args, "-coord", addr)) })
	}
	modeInList := func() string {
		for _, line := range strings.Split(run("list"), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 || f[0] != id {
				continue
			}
			// The schedule column can be several words ("every 5m"), so
			// find the mode word rather than a fixed column.
			for _, w := range f[1:] {
				if w == "lease" || w == "claims" || w == "rollback" {
					return w
				}
			}
		}
		t.Fatalf("cron %s not in list", id)
		return ""
	}

	if got := modeInList(); got != "lease" {
		t.Fatalf("initial mode = %q", got)
	}
	if out := run("set", id); !strings.Contains(out, "dry run") || modeInList() != "lease" {
		t.Fatalf("set without --yes must not change anything:\n%s", out)
	}
	if out := run("set", id, "--yes"); !strings.Contains(out, "switched to claims: ["+id+"]") {
		t.Fatalf("set --yes:\n%s", out)
	}
	if got := modeInList(); got != "claims" {
		t.Fatalf("mode after set = %q", got)
	}
	if out := run("rollback", "all"); !strings.Contains(out, "dry run") || modeInList() != "claims" {
		t.Fatalf("rollback without --yes must not change anything:\n%s", out)
	}
	out := run("rollback", "all", "--yes")
	if !strings.Contains(out, "rolled back: ["+id+"]") || !strings.Contains(out, "45s") {
		t.Fatalf("rollback all --yes:\n%s", out)
	}
	if got := modeInList(); got != "rollback" {
		t.Fatalf("mode after rollback = %q", got)
	}
}
