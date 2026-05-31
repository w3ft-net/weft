package api

import (
	"strings"
	"testing"

	"github.com/kevlarcon/weft/auth"
)

// TestMaybeHandleTeamCommand_Routing covers the interception boundary —
// which pipelines are claimed as team commands vs forwarded to the
// coordinator — without needing a store (the claimed cases here stop at
// the owner check or the parse guard).
func TestMaybeHandleTeamCommand_Routing(t *testing.T) {
	s := &Server{}
	member := &auth.Session{Email: "alice@x.test", IsOwner: false, ActiveCustomerName: "acme"}

	cases := []struct {
		pipeline    string
		wantHandled bool
		wantErrSub  string // substring expected in err when handled
	}{
		{"cat api.log | grep ERROR", false, ""},
		{"range 1d..", false, ""},
		{"", false, ""},
		// Team commands inside shell plumbing fall through to the
		// normal pipeline path rather than being claimed.
		{"id | wc -l", false, ""},
		{"adduser alice@x.test payments && echo hi", false, ""},
		// A non-owner trying to mutate is claimed and rejected here,
		// before any store access.
		{"adduser bob@x.test", true, "only the owner"},
		{"deluser bob@x.test payments", true, "only the owner"},
	}
	for _, c := range cases {
		handled, _, err := s.maybeHandleTeamCommand(member, c.pipeline)
		if handled != c.wantHandled {
			t.Errorf("%q: handled=%v want %v", c.pipeline, handled, c.wantHandled)
			continue
		}
		if c.wantErrSub != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErrSub) {
				t.Errorf("%q: err=%v want substring %q", c.pipeline, err, c.wantErrSub)
			}
		}
	}
}
