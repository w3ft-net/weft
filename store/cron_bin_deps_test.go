package store

import (
	"slices"
	"testing"
)

// A cron's scripts, and the scripts those call, are recorded as it is saved, so
// a write to any of them can be checked against the crons that run it.
func TestCronBinDeps_FollowTheCronAndItsScripts(t *testing.T) {
	db := testOpen(t)
	org, _ := db.FindOrCreateOrg("bin-deps")
	alpha, _ := db.FindOrCreateNamespace(org.ID, "alpha")
	save := func(name, src string) {
		t.Helper()
		if err := db.SaveShellFunction(org.ID, "", name, src, "bash"); err != nil {
			t.Fatal(err)
		}
	}
	using := func(script string) []string {
		t.Helper()
		crons, err := db.CronsUsingBin(org.ID, script)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range crons {
			names = append(names, c.Name)
		}
		slices.Sort(names)
		return names
	}
	save("scan", "cat /log/alpha/api/main.log | grep ERROR")
	save("outer", "/bin/scan | wc -l")
	save("helper", "echo hi")

	if err := db.SaveCronWatch(org.ID, alpha.ID, "", "errors", "every 5m", "/bin/outer", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveCronWatch(org.ID, alpha.ID, "", "plain", "every 5m", "echo 1", true, nil); err != nil {
		t.Fatal(err)
	}
	if got := using("outer"); !slices.Equal(got, []string{"errors"}) {
		t.Errorf("crons using outer = %v", got)
	}
	if got := using("scan"); !slices.Equal(got, []string{"errors"}) {
		t.Errorf("crons using scan (called by outer) = %v", got)
	}
	if got := using("helper"); len(got) != 0 {
		t.Errorf("crons using helper = %v, want none", got)
	}

	// A script that starts calling another puts that one in the chain.
	save("scan", "/bin/helper; cat /log/alpha/api/main.log")
	if got := using("helper"); !slices.Equal(got, []string{"errors"}) {
		t.Errorf("after scan calls helper: crons using helper = %v", got)
	}

	// Saving the cron without the script drops it; deleting the cron drops the rest.
	if err := db.SaveCronWatch(org.ID, alpha.ID, "", "errors", "every 5m", "echo 2", true, nil); err != nil {
		t.Fatal(err)
	}
	if got := using("outer"); len(got) != 0 {
		t.Errorf("after the cron stopped calling outer: %v", got)
	}
	if err := db.SaveCronWatch(org.ID, alpha.ID, "", "errors", "every 5m", "/bin/scan", true, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteCronWatch(org.ID, alpha.ID, "errors"); err != nil {
		t.Fatal(err)
	}
	if got := using("scan"); len(got) != 0 {
		t.Errorf("a deleted cron still counts as using scan: %v", got)
	}
}
