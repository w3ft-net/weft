package mounts

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"github.com/w3ft-net/weft/identity"
)

// /etc is readable by every member; writing needs the
// matching group. /cron.d and /bin depend on
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
		member := tc.mount(Config{Principal: nobody{}, Authz: grants{}})
		manager := tc.mount(Config{Principal: nobody{}, Authz: grants{identity.ActionManageBin: true, identity.ActionManageCron: true, identity.ActionManageEtc: true}})
		for _, want := range []shellcore.Perm{r, x, r | x} {
			if err := member.Authorize(context.Background(), tc.rel, want); err != nil {
				t.Errorf("%s: member denied %03b: %v", tc.name, want, err)
			}
		}
		if err := member.Authorize(context.Background(), tc.rel, w); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("%s: member write: err = %v, want fs.ErrPermission", tc.name, err)
		}
		if err := member.Authorize(context.Background(), tc.rel, r|w); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("%s: member read+write: err = %v, want fs.ErrPermission", tc.name, err)
		}
		if err := manager.Authorize(context.Background(), tc.rel, r|w|x); err != nil {
			t.Errorf("%s: manager denied: %v", tc.name, err)
		}
	}
}
