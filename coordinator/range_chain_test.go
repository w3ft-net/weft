package coordinator

import (
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/compute"
)

// The clipboard deep link and the web shell's range chip send
// `range <spec>; <command>`; the range must apply and the command must run.
func TestExecute_RangeThenCommand(t *testing.T) {
	db, org := testStoreAndCustomer(t)
	alice, _ := db.CreateUser("alice-rc@example.test")
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db})
	out := gitRun(t, c, org, alice.ID, "range 2026-10-11T04:51:39Z..2026-10-12T04:51:39Z; echo ran")
	if strings.Contains(out, "cannot parse") || !strings.Contains(out, "ran") {
		t.Errorf("out = %q", out)
	}
}
