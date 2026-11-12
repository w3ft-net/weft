package hostedapi

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/notices"
)

func TestGraduationNotice_DeliveredInShellClearsBanner(t *testing.T) {
	srv, s := testServer(t)
	cust, _ := s.FindOrCreateOrg(fmt.Sprintf("notices-grad-%d", os.Getpid()))
	user, _ := permsSeed(t, s, cust, orgs.RoleMember, "notices-grad")

	if err := s.EmitNotice(user.ID, GraduationNoticeKey, "success", nil); err != nil {
		t.Fatal(err)
	}
	if ids := srv.pendingGraduationIDs(context.Background(), user.ID); len(ids) != 1 {
		t.Fatalf("banner should be pending; got %d ids", len(ids))
	}

	// The shell drains it.
	drained := notices.NewQueue(s, user.ID).Drain(context.Background())
	if len(drained) != 1 || drained[0].Key != GraduationNoticeKey {
		t.Fatalf("shell drain: got %+v", drained)
	}

	if ids := srv.pendingGraduationIDs(context.Background(), user.ID); len(ids) != 0 {
		t.Errorf("banner still pending after shell delivery; got %d ids", len(ids))
	}
}
