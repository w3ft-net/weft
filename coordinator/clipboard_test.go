//go:build !defaultshape

package coordinator

import (
	"regexp"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/store"
)

func TestClipboardCommand_MakesAShortLink(t *testing.T) {
	db, org := testStoreAndCustomer(t)
	alice, _ := db.CreateUser("alice-clip@example.test")
	db.SaveShellFunction(org, alice.ID, "scan", "echo hi", "bash")
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db, ShareURLBase: "https://share.test/"})

	out := gitRun(t, c, org, alice.ID, "clipboard /bin/scan -- --app=loom --since=1h")
	m := regexp.MustCompile(`copied: (https://share\.test/s/([0-9A-Za-z]{10}))\n`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no short link in:\n%q", out)
	}
	if strings.Contains(m[1], "?") || strings.Contains(m[1], "bin=") {
		t.Errorf("link is not clean: %s", m[1])
	}
	if !strings.Contains(out, "\x1b]52;c;") {
		t.Errorf("no OSC-52 clipboard escape in %q", out)
	}
	l, err := db.GetSharedLink(org, m[2])
	if err != nil {
		t.Fatal(err)
	}
	if l.Bin != "/bin/scan" || l.Args != "--app=loom --since=1h" || !strings.Contains(l.RangeSpec, "..") {
		t.Errorf("stored link = %+v", l)
	}
	if l.CreatedByUserID == nil || *l.CreatedByUserID != alice.ID {
		t.Errorf("creator = %v", l.CreatedByUserID)
	}

	// An explicit range overrides the current one.
	out = gitRun(t, c, org, alice.ID, "clipboard /bin/scan --range=24h..")
	m2 := regexp.MustCompile(`/s/([0-9A-Za-z]{10})`).FindStringSubmatch(out)
	if m2 == nil {
		t.Fatalf("no link in %q", out)
	}
	if l2, _ := db.GetSharedLink(org, m2[1]); l2 == nil || l2.RangeSpec != "24h.." {
		t.Errorf("explicit range = %+v", l2)
	}
}

func TestClipboardCommand_Refusals(t *testing.T) {
	db, org := testStoreAndCustomer(t)
	alice, _ := db.CreateUser("alice-clip2@example.test")
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	for line, want := range map[string]string{
		"clipboard":                           "usage: clipboard",
		"clipboard /tmp/x":                    "must be a /bin/<name> path",
		"clipboard /bin/nope":                 "no such script",
		"clipboard /bin/scan --bogus":         "unknown option",
		"clipboard /bin/scan --range=garbage": "range",
	} {
		if out := gitRun(t, c, org, alice.ID, line); !strings.Contains(out, want) {
			t.Errorf("%s: %q, want %q", line, out, want)
		}
	}
	var n int64
	db.DB().WithContext(store.WithOrg(t.Context(), org)).Model(&store.SharedLink{}).Count(&n)
	if n != 0 {
		t.Errorf("refused commands stored %d links", n)
	}
}
