package coordinator

import (
	"context"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"
)

// committedInteriorFixture is the interior-query fixture over a pooled,
// committed store (see newInteriorQueryFixtureOn), cleaned up afterwards.
func committedInteriorFixture(t *testing.T) (*Coordinator, *store.Store, store.OrgID) {
	t.Helper()
	db, err := store.Open(store.TestDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	org, err := db.FindOrCreateOrg("weftfuncs-test")
	if err != nil {
		t.Fatal(err)
	}
	adb := db.DB().WithContext(store.WithoutTenantScope(context.Background()))
	t.Cleanup(func() {
		// The fixture's shard row (shard_services cascades). Left behind, it
		// is a phantom INGESTING shard for every later test on this pool slot.
		adb.Exec(`DELETE FROM shard_records WHERE id = 'interior-test'`)
		adb.Unscoped().Where("org_id = ?", org.ID).Delete(&store.ShellFunction{})
		adb.Unscoped().Where("org_id = ?", org.ID).Delete(&store.Namespace{})
		adb.Exec(`DELETE FROM organizations WHERE id = ?`, org.ID)
		db.Close()
	})
	return newInteriorQueryFixtureOn(t, db, org.ID)
}

// A /bin script used as a later stage after a log read runs where /bin
// exists (warp over shellfs), fed by the query. The query half goes to
// compute, whose stateless worker has no /bin; the script word used to be
// shipped there and failed with "compute exit 127: <name>: command not
// found". The pipeline is split at the first stage that names a script.
func TestBinScriptAfterLogRead(t *testing.T) {
	c, db, custID := committedInteriorFixture(t)

	// api has 3 rows, 2 of them ERROR.
	if err := db.SaveShellFunction(custID, "", "count", "wc -l", "bash"); err != nil {
		t.Fatalf("save bash script: %v", err)
	}
	if err := db.SaveShellFunction(custID, "", "luacount", "local n = 0\nfor _ in lines() do n = n + 1 end\nprint(n)", "lua"); err != nil {
		t.Fatalf("save lua script: %v", err)
	}

	cases := []struct{ name, line, want string }{
		{"bash filter as last stage", `cat /log/api.log | grep ERROR | count`, "2"},
		{"lua filter as last stage", `cat /log/api.log | grep ERROR | luacount`, "2"},
		{"lua filter, whole log", `cat /log/api.log | luacount`, "3"},
		{"stages after the script", `cat /log/api.log | count | awk '{ print $1 + 10 }'`, "13"},
		{"compute stage before the script", `cat /log/api.log | grep -v INFO | sort | count`, "2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := c.Execute(context.Background(), tc.line, lastHourRange(), hosted.Actor{Org: custID, User: ""})
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			d := drainExec(t, res)
			if got := strings.TrimSpace(d.Text); got != tc.want {
				t.Errorf("%s\n got %q (stderr %q)\nwant %q", tc.line, got, d.Stderr, tc.want)
			}
		})
	}
}

// A redirect on the last stage belongs to that stage. With the script
// split off from the query, the redirect must stay on the script's output,
// not be lifted onto the query callback.
func TestBinScriptAfterLogRead_RedirectStaysOnLastStage(t *testing.T) {
	c, db, custID := committedInteriorFixture(t)
	if err := db.SaveShellFunction(custID, "", "count", "wc -l", "bash"); err != nil {
		t.Fatalf("save script: %v", err)
	}
	res, err := c.Execute(context.Background(), `cat /log/api.log | grep ERROR | count > /tmp/n`, lastHourRange(), hosted.Actor{Org: custID, User: ""})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if d := drainExec(t, res); strings.TrimSpace(d.Text) != "" || d.Stderr != "" {
		t.Fatalf("redirected output leaked: text %q stderr %q", d.Text, d.Stderr)
	}
	res, err = c.Execute(context.Background(), `cat /tmp/n`, lastHourRange(), hosted.Actor{Org: custID, User: ""})
	if err != nil {
		t.Fatalf("Execute cat: %v", err)
	}
	if got := strings.TrimSpace(drainExec(t, res).Text); got != "2" {
		t.Errorf("/tmp/n = %q, want the script's output 2", got)
	}
}
