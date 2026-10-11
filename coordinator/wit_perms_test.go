package coordinator

import (
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/weft/store"
)

// grantWitPerms gives the user the binmgr/cronmgr bits, the way an
// owner's promotion materializes them.
func grantWitPerms(t *testing.T, wf *WitFuncs, fn, watch bool) {
	t.Helper()
	err := wf.Store.DB().WithContext(store.WithOrg(t.Context(), wf.OrgID)).Create(&store.OrgPermission{
		OrgID: wf.OrgID, UserID: wf.UserID,
		CanManageFunctions: fn, CanManageWatches: watch,
	}).Error
	if err != nil {
		t.Fatal(err)
	}
}

func TestWit_Checkout_DeniedWithoutBinmgr(t *testing.T) {
	wf, _ := witHelper(t, "perm-bin", 3)
	_, stderr, code := runWit(t, wf, "checkout", "/bin/scan", "HEAD^2")
	if code == 0 {
		t.Fatalf("checkout without binmgr succeeded")
	}
	if !strings.Contains(stderr, "binmgr") {
		t.Errorf("stderr = %q, want a binmgr denial", stderr)
	}
	fn, _ := wf.Store.GetShellFunction(wf.OrgID, "scan")
	if fn.Source != "echo v3" {
		t.Errorf("source changed to %q despite denial", fn.Source)
	}
}

func TestWit_Checkout_DeniedWithoutCronmgr(t *testing.T) {
	wf, _ := witHelper(t, "perm-cron", 0)
	for _, p := range []string{"echo a", "echo b"} {
		if err := wf.Store.SaveCronWatch(wf.OrgID, wf.UserID, "tick", "*/10 * * * *", p, nil); err != nil {
			t.Fatal(err)
		}
	}
	_, stderr, code := runWit(t, wf, "checkout", "/cron.d/tick", "HEAD^")
	if code == 0 || !strings.Contains(stderr, "cronmgr") {
		t.Fatalf("code=%d stderr=%q, want cronmgr denial", code, stderr)
	}
	w, _ := wf.Store.GetCronWatch(wf.OrgID, "tick")
	if w.Pipeline != "echo b" {
		t.Errorf("pipeline changed to %q despite denial", w.Pipeline)
	}
}

func TestWit_Checkout_ClassifiesLuaBin(t *testing.T) {
	wf, _ := witHelper(t, "perm-lua", 0)
	grantWitPerms(t, wf, true, true)
	if err := wf.Store.SaveShellFunction(wf.OrgID, wf.UserID, "x", "return 1", "lua"); err != nil {
		t.Fatal(err)
	}
	if err := wf.Store.SaveShellFunction(wf.OrgID, wf.UserID, "x", "echo hi", "bash"); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := runWit(t, wf, "checkout", "/bin/x", "HEAD^"); code != 0 {
		t.Fatalf("checkout: %q", stderr)
	}
	fn, _ := wf.Store.GetShellFunction(wf.OrgID, "x")
	if fn.Language != "lua" || fn.Source != "return 1" {
		t.Errorf("got lang=%q source=%q, want lua/return 1", fn.Language, fn.Source)
	}
}

func TestWit_Checkout_CronEnforcesTierInterval(t *testing.T) {
	wf, _ := witHelper(t, "perm-interval", 0)
	grantWitPerms(t, wf, true, true)
	if err := wf.Store.CreateSubscription(&store.Subscription{
		ID: "sub-wit-interval", OrgID: wf.OrgID, Tier: "swatch",
		Status: "active", StartsAt: time.Now().Add(-time.Hour), CreatedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// Seed a too-frequent schedule directly (bypassing the mount), then
	// replace it; checking out the old version must hit the tier limit.
	if err := wf.Store.SaveCronWatch(wf.OrgID, wf.UserID, "fast", "* * * * *", "echo a", nil); err != nil {
		t.Fatal(err)
	}
	if err := wf.Store.SaveCronWatch(wf.OrgID, wf.UserID, "fast", "0 * * * *", "echo b", []byte(`{"stale":true}`)); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runWit(t, wf, "checkout", "/cron.d/fast", "HEAD^")
	if code == 0 {
		t.Fatalf("checkout of sub-minimum interval succeeded")
	}
	if !strings.Contains(stderr, "minimum schedule interval") {
		t.Errorf("stderr = %q, want interval refusal", stderr)
	}
}
