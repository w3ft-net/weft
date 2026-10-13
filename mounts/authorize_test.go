package mounts

import (
	"errors"
	"io/fs"
	"testing"

	"github.com/graybeardcorp/warp/pkg/shellcore"
)

// /bin, /cron.d and /etc are readable by every member; writing needs the
// matching group, and /etc/passwd is never writable.
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
		{"/bin", func(c Config) shellcore.AuthMount { return &BinMount{cfg: c} }, "deploy"},
		{"/cron.d", func(c Config) shellcore.AuthMount { return &CronMount{cfg: c} }, "nightly"},
		{"/etc", func(c Config) shellcore.AuthMount { return &EtcMount{cfg: c} }, "notify.yaml"},
	}
	for _, tc := range cases {
		member := tc.mount(Config{})
		manager := tc.mount(Config{CanManageFunctions: true, CanManageWatches: true, CanManageEtc: true})
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

	etc := &EtcMount{cfg: Config{CanManageEtc: true}}
	if err := etc.Authorize("passwd", r); err != nil {
		t.Errorf("/etc/passwd read: %v", err)
	}
	if err := etc.Authorize("passwd", w); err == nil {
		t.Error("/etc/passwd write allowed for a configmgr member")
	}
}
