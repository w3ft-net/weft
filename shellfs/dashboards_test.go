package shellfs

import (
	"io/fs"
	"strings"
	"testing"
)

const sampleDashboardYAML = `title: Test
sections:
  - title: Errors
    tiles:
      - kind: sparkline
        cron: error-rate-hourly
        size: md
`

// seedDashboardWatch creates a cron watch with the given name so dashboard
// validation has a real reference target.
func seedDashboardWatch(t *testing.T, fs *ShellFS, name string) {
	t.Helper()
	body := "# schedule: every 5m\necho 1\n"
	if err := fs.WriteFile("/cron.d/"+name, []byte(body)); err != nil {
		t.Fatalf("seed cron watch %q: %v", name, err)
	}
}

func TestDashboards_WriteReadStat(t *testing.T) {
	db := testStore(t)
	cust, err := db.FindOrCreateOrg("dash-fs-test")
	if err != nil {
		t.Fatal(err)
	}
	sfs := New(db, cust.ID, []string{"api"}, WithTier("swatch"), WithUser("test-owner", ownerPerms()))

	seedDashboardWatch(t, sfs, "error-rate-hourly")

	if err := sfs.WriteFile("/etc/dashboards/prod.yaml", []byte(sampleDashboardYAML)); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := sfs.ReadFile("/etc/dashboards/prod.yaml")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != sampleDashboardYAML {
		t.Errorf("read body mismatch:\n got: %q\nwant: %q", got, sampleDashboardYAML)
	}

	info, err := sfs.Stat("/etc/dashboards/prod.yaml")
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if info.Name() != "prod.yaml" || info.IsDir() {
		t.Errorf("stat got name=%q dir=%v", info.Name(), info.IsDir())
	}

	dirInfo, err := sfs.Stat("/etc/dashboards")
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if !dirInfo.IsDir() {
		t.Errorf("/etc/dashboards should be a directory")
	}
}

func TestDashboards_ReadDir(t *testing.T) {
	db := testStore(t)
	cust, err := db.FindOrCreateOrg("dash-readdir-test")
	if err != nil {
		t.Fatal(err)
	}
	sfs := New(db, cust.ID, []string{"api"}, WithTier("swatch"), WithUser("test-owner", ownerPerms()))

	seedDashboardWatch(t, sfs, "watch-a")
	seedDashboardWatch(t, sfs, "watch-b")

	if err := sfs.WriteFile("/etc/dashboards/alpha.yaml", []byte("tiles:\n  - {kind: counter, cron: watch-a}\n")); err != nil {
		t.Fatalf("write alpha: %v", err)
	}
	if err := sfs.WriteFile("/etc/dashboards/beta.yaml", []byte("tiles:\n  - {kind: counter, cron: watch-b}\n")); err != nil {
		t.Fatalf("write beta: %v", err)
	}

	entries, err := sfs.ReadDir("/etc/dashboards")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	got := names(entries)
	want := []string{"alpha.yaml", "beta.yaml"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("listing got %v, want %v", got, want)
	}

	etcEntries, err := sfs.ReadDir("/etc")
	if err != nil {
		t.Fatalf("readdir /etc: %v", err)
	}
	if !contains(names(etcEntries), "dashboards") {
		t.Errorf("/etc listing missing `dashboards/`: %v", names(etcEntries))
	}
}

func TestDashboards_WriteRejectsBadYAML(t *testing.T) {
	db := testStore(t)
	cust, err := db.FindOrCreateOrg("dash-bad-yaml")
	if err != nil {
		t.Fatal(err)
	}
	sfs := New(db, cust.ID, []string{"api"}, WithTier("swatch"), WithUser("test-owner", ownerPerms()))

	err = sfs.WriteFile("/etc/dashboards/broken.yaml", []byte("tiles:\n  - kind: pizza\n    cron: nope\n"))
	if err == nil {
		t.Fatal("expected error for unknown kind")
	}
	if !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("error: %v", err)
	}
}

func TestDashboards_WriteRejectsMissingWatch(t *testing.T) {
	db := testStore(t)
	cust, err := db.FindOrCreateOrg("dash-missing-watch")
	if err != nil {
		t.Fatal(err)
	}
	sfs := New(db, cust.ID, []string{"api"}, WithTier("swatch"), WithUser("test-owner", ownerPerms()))

	err = sfs.WriteFile("/etc/dashboards/orphan.yaml", []byte("tiles:\n  - kind: counter\n    cron: does-not-exist\n"))
	if err == nil {
		t.Fatal("expected error for missing watch")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error: %v", err)
	}
}

func TestDashboards_Remove(t *testing.T) {
	db := testStore(t)
	cust, err := db.FindOrCreateOrg("dash-rm-test")
	if err != nil {
		t.Fatal(err)
	}
	sfs := New(db, cust.ID, []string{"api"}, WithTier("swatch"), WithUser("test-owner", ownerPerms()))
	seedDashboardWatch(t, sfs, "w")

	if err := sfs.WriteFile("/etc/dashboards/x.yaml", []byte("tiles:\n  - {kind: counter, cron: w}\n")); err != nil {
		t.Fatal(err)
	}
	if err := sfs.Remove("/etc/dashboards/x.yaml"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := sfs.ReadFile("/etc/dashboards/x.yaml"); err == nil {
		t.Error("expected file gone after remove")
	}
}

func names(entries []fs.DirEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name()
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// The commit-message popup records through AmendVersionMessage, so /etc's
// versioned files must accept it.
func TestEtc_AmendVersionMessage(t *testing.T) {
	db := testStore(t)
	cust, err := db.FindOrCreateOrg("etc-amend-test")
	if err != nil {
		t.Fatal(err)
	}
	sfs := New(db, cust.ID, []string{"api"}, WithTier("swatch"), WithUser("test-owner", ownerPerms()))
	seedDashboardWatch(t, sfs, "error-rate-hourly")
	if err := sfs.WriteFile("/etc/dashboards/prod.yaml", []byte(sampleDashboardYAML)); err != nil {
		t.Fatal(err)
	}
	if err := sfs.WriteFile("/etc/notify.yaml", []byte("system:\n  email:\n    - \"owner@loom.test\"\nops:\n  email:\n    - \"ops@loom.test\"\n")); err != nil {
		t.Fatal(err)
	}

	if err := sfs.AmendVersionMessage("/etc/dashboards/prod.yaml", "first dashboard"); err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if err := sfs.AmendVersionMessage("/etc/notify.yaml", "first aliases"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	var got []string
	db.DB().Raw("SELECT message FROM dashboards WHERE org_id = ? UNION ALL SELECT message FROM notify_configs WHERE org_id = ?", cust.ID, cust.ID).Scan(&got)
	if strings.Join(got, "|") != "first dashboard|first aliases" {
		t.Errorf("messages = %v", got)
	}
	if err := sfs.AmendVersionMessage("/etc/passwd", "nope"); err == nil {
		t.Error("/etc/passwd is not versioned")
	}
}
