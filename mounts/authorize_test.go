//go:build !defaultshape

package mounts

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
	"github.com/graybeardcorp/weft/tenancy"
)

// /etc is readable by every member; writing needs the
// matching group, and /etc/passwd is never writable. /cron.d and /bin depend on
// the store (a path's namespace, the crons that run a script): see
// TestCronMount_NamespaceOwnership and TestBinMount_WritesFollowTheCronsThatRunTheScript.
func TestAuthorize_WriteNeedsGroup(t *testing.T) {
	const (
		r = shellcore.PermRead
		w = shellcore.PermWrite
		x = shellcore.PermExec
	)
	cases := []struct {
		name  string
		mount func(Config) shellcore.AuthMount
		rel   string
	}{
		{"/etc", func(c Config) shellcore.AuthMount { return &EtcMount{cfg: c} }, "notify.yaml"},
	}
	for _, tc := range cases {
		member := tc.mount(Config{Actor: nobody{}, Authz: grants{}})
		manager := tc.mount(Config{Actor: nobody{}, Authz: grants{tenancy.ActionManageBin: true, tenancy.ActionManageCron: true, tenancy.ActionManageEtc: true}})
		for _, want := range []shellcore.Perm{r, x, r | x} {
			if err := member.Authorize(tc.rel, want); err != nil {
				t.Errorf("%s: member denied %03b: %v", tc.name, want, err)
			}
		}
		if err := member.Authorize(tc.rel, w); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("%s: member write: err = %v, want fs.ErrPermission", tc.name, err)
		}
		if err := member.Authorize(tc.rel, r|w); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("%s: member read+write: err = %v, want fs.ErrPermission", tc.name, err)
		}
		if err := manager.Authorize(tc.rel, r|w|x); err != nil {
			t.Errorf("%s: manager denied: %v", tc.name, err)
		}
	}

	etc := &EtcMount{cfg: Config{Actor: nobody{}, Authz: grants{tenancy.ActionManageEtc: true}}}
	if err := etc.Authorize("passwd", r); err != nil {
		t.Errorf("/etc/passwd read: %v", err)
	}
	if err := etc.Authorize("passwd", w); err == nil {
		t.Error("/etc/passwd write allowed for a configmgr member")
	}
}
