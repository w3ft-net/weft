package mounts

import (
	"context"
	"errors"
	"strings"
	"testing"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/mem"
	"github.com/w3ft-net/weft/dashboard"
	"github.com/w3ft-net/weft/identity"
)

// dashboardFS is a filesystem with /etc/dashboards over SQLite, acting as a
// configmgr member (or not), with a numeric cron ops/errors and a text cron
// ops/holders.
func dashboardFS(t *testing.T, manager bool) (*shellcore.MountFS, *mem.Dashboards, Config) {
	t.Helper()
	cfg := ownerCfg(t, []string{"ops/web"})
	st := mem.NewDashboards()
	if manager {
		cfg.Authz = grants{identity.ActionManageEtc: true}
	}
	cfg.EtcExtensions = func(c Config) []shellcore.Mount { return []shellcore.Mount{Dashboards(c, st)} }

	ns, ok, err := cfg.Sources.GetNamespace(cfg.Owner, "ops")
	if err != nil || !ok {
		t.Fatalf("namespace ops: %v %v", ok, err)
	}
	for name, typ := range map[string]string{"errors": cron.ValueNumber, "holders": cron.ValueText} {
		spec := cron.WatchSpec{Schedule: "every 5m", Pipeline: "echo 1", Enabled: true, Compiled: cron.Compiled{ValueType: typ}}
		if err := cfg.CronAuthor.Crons.SaveWatch(cfg.Owner, ns.ID, "", name, spec); err != nil {
			t.Fatal(err)
		}
	}
	return New(cfg), st, cfg
}

func writeDashboard(t *testing.T, mfs *shellcore.MountFS, name, body string) error {
	t.Helper()
	return mfs.WriteFile(context.Background(), "/etc/dashboards/"+name+".yaml", []byte(body))
}

// Writing a dashboard takes the configmgr group, the way every /etc write does;
// reading is open to every member.
func TestDashboards_FollowTheEtcGroup(t *testing.T) {
	const r, w = shellcore.PermRead, shellcore.PermWrite
	member, _, _ := dashboardFS(t, false)
	manager, _, _ := dashboardFS(t, true)
	if err := member.Access(context.Background(), "/etc/dashboards/ops.yaml", r); err != nil {
		t.Errorf("member read: %v", err)
	}
	if err := member.Access(context.Background(), "/etc/dashboards/ops.yaml", w); err == nil || errors.Is(err, shellcore.ErrReadOnly) {
		t.Errorf("member write = %v, want the configmgr denial", err)
	}
	if err := manager.Access(context.Background(), "/etc/dashboards/ops.yaml", w); err != nil {
		t.Errorf("manager write: %v", err)
	}
}

func TestDashboards_SaveReadListDelete(t *testing.T) {
	mfs, st, cfg := dashboardFS(t, true)
	body := "title: Ops\ntiles:\n  - {kind: sparkline, cron: ops/errors}\n"
	if err := writeDashboard(t, mfs, "ops", body); err != nil {
		t.Fatal(err)
	}
	got, err := mfs.ReadFile(context.Background(), "/etc/dashboards/ops.yaml")
	if err != nil || string(got) != body {
		t.Fatalf("read back = %q, %v", got, err)
	}
	if d, err := st.Get(cfg.Owner, "ops"); err != nil || d.YAMLBody != body {
		t.Fatalf("stored = %+v, %v", d, err)
	}
	entries, err := mfs.ReadDir(context.Background(), "/etc/dashboards")
	if err != nil || len(entries) != 1 || entries[0].Name() != "ops.yaml" {
		t.Fatalf("listing = %v, %v", entries, err)
	}
	if err := mfs.Remove(context.Background(), "/etc/dashboards/ops.yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := mfs.ReadFile(context.Background(), "/etc/dashboards/ops.yaml"); err == nil {
		t.Error("a deleted dashboard still reads")
	}
}

func TestDashboards_RefuseWhatWouldNotRender(t *testing.T) {
	mfs, st, cfg := dashboardFS(t, true)
	for _, tc := range []struct{ name, body, want string }{
		{"not yaml of ours", "tiles:\n  - {kind: pie, cron: ops/errors}\n", "unknown kind"},
		{"no such cron", "tiles:\n  - {kind: counter, cron: ops/nope}\n", `cron "ops/nope" does not exist`},
		{"unreadable namespace reads as missing", "tiles:\n  - {kind: counter, cron: other/x}\n", `cron "other/x" does not exist`},
		{"number tile on a text cron", "tiles:\n  - {kind: counter, cron: ops/holders}\n", "a counter tile draws numbers"},
		{"windowed table on a text cron", "tiles:\n  - {kind: table, cron: ops/holders, range: 1h..}\n", "can't aggregate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := writeDashboard(t, mfs, "bad", tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if list, _ := st.List(cfg.Owner); len(list) != 0 {
		t.Errorf("a refused dashboard was stored: %+v", list)
	}
	// A table shows text, so it takes a text cron.
	if err := writeDashboard(t, mfs, "leases", "tiles:\n  - {kind: table, cron: ops/holders}\n"); err != nil {
		t.Errorf("table on a text cron: %v", err)
	}
}

func TestDashboardReaders_NameTheNumericReaders(t *testing.T) {
	mfs, st, cfg := dashboardFS(t, true)
	if err := writeDashboard(t, mfs, "ops", "tiles:\n  - {kind: sparkline, cron: ops/errors}\n  - {kind: table, cron: ops/errors}\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeDashboard(t, mfs, "other", "tiles:\n  - {kind: table, cron: ops/holders}\n"); err != nil {
		t.Fatal(err)
	}
	got, err := dashboard.Readers(st)(cfg.Owner, "ops/errors")
	if err != nil || len(got) != 1 || got[0] != "/etc/dashboards/ops.yaml (sparkline)" {
		t.Errorf("readers = %v, %v", got, err)
	}
	if got, _ := dashboard.Readers(st)(cfg.Owner, "ops/holders"); len(got) != 0 {
		t.Errorf("a table is not a numeric reader: %v", got)
	}
}

func TestDashboardCronRenamed_WarnsOfTilesThatNameTheOldPath(t *testing.T) {
	mfs, st, cfg := dashboardFS(t, true)
	// The writer must read a namespace named like the cron for a bare name to pass
	// the save check, so seed the store directly.
	if err := st.Save(cfg.Owner, "", "ops", "tiles:\n  - {kind: counter, cron: errors}\n"); err != nil {
		t.Fatal(err)
	}
	_ = mfs
	err := DashboardCronRenamed(st)(cfg, "errors", "ops/errors")
	if err == nil || !strings.Contains(err.Error(), "dashboards ops still name it") {
		t.Fatalf("err = %v", err)
	}
	if err := DashboardCronRenamed(st)(cfg, "other", "ops/other"); err != nil {
		t.Errorf("a cron no dashboard names: %v", err)
	}
}
