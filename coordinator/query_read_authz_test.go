package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/store"
)

// A log query must be held to the same per-namespace read permission the
// shell's /log listing enforces: ls hides a namespace you can't read, and a
// pipeline that names it must not read it either. The empty user is the
// system identity (crons, internal callers) and reads everything.

type readFixture struct {
	c    *Coordinator
	cust string

	owner, reader, none string // user ids: org owner / reads alpha only / reads nothing
}

func newReadFixture(t *testing.T) *readFixture {
	t.Helper()
	db, custID := testStoreAndCustomer(t)
	cust, _ := db.GetOrgByID(custID)
	alpha, err := db.FindOrCreateNamespace(custID, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FindOrCreateNamespace(custID, "beta"); err != nil {
		t.Fatal(err)
	}
	mk := func(name string, role orgs.Role) string {
		u, err := db.CreateVerifiedUser(fmt.Sprintf("%s-readauthz@example.test", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AddTeamMemberWithEmail(custID, u.ID, u.Email, role); err != nil {
			t.Fatal(err)
		}
		return u.ID
	}
	f := &readFixture{
		c:      NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, Store: db}),
		cust:   cust.Name,
		owner:  mk("owner", store.RoleOwner),
		reader: mk("reader", orgs.RoleMember),
		none:   mk("none", orgs.RoleMember),
	}
	if err := db.UpsertNamespacePermission(&store.NamespacePermission{UserID: f.reader, NamespaceID: alpha.ID, CanRead: true}); err != nil {
		t.Fatal(err)
	}
	return f
}

// run executes the pipeline as user and returns the first error, from the call,
// from draining its stream, or from its stderr. There are no shards here, so a query that gets
// past authorization fails later with something other than a permission error.
func (f *readFixture) run(t *testing.T, user, pipeline string) error {
	t.Helper()
	tr := TimeRange{Start: time.Unix(0, 0), End: time.Now().Add(time.Hour)}
	res, err := f.c.Execute(context.Background(), pipeline, tr, f.cust, user)
	if err != nil {
		return err
	}
	if res == nil {
		return nil
	}
	d, err := res.Drain(context.Background())
	if err != nil {
		return err
	}
	// A failed query reports on stderr, as it does to a shell user.
	if msg := strings.TrimSpace(d.Stderr); msg != "" {
		return errors.New(msg)
	}
	return nil
}

func refused(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "unknown source") || strings.Contains(err.Error(), "every namespace"))
}

func TestQuery_ReadsOnlyNamespacesTheUserMayRead(t *testing.T) {
	f := newReadFixture(t)
	cases := []struct {
		name, user, pipeline string
		refuse               bool
	}{
		{"reader on a namespace they can read", f.reader, "cat /log/alpha/api/main.log", false},
		{"reader on a namespace they cannot", f.reader, "cat /log/beta/api/main.log", true},
		{"no-permission member on any namespace", f.none, "cat /log/alpha/api/main.log", true},
		{"owner reads every namespace", f.owner, "cat /log/beta/api/main.log", false},
		{"system identity reads every namespace", "", "cat /log/beta/api/main.log", false},
		{"nonexistent namespace reads like an unreadable one", f.reader, "cat /log/nope/api/main.log", true},
		// The planner reports every namespace a pipeline names, not just the
		// first; naming one the user can't read taints the whole query.
		{"reader mixing a readable and an unreadable namespace", f.reader, "cat /log/alpha/api/main.log /log/beta/api/main.log", true},
		{"owner mixing namespaces", f.owner, "cat /log/alpha/api/main.log /log/beta/api/main.log", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := f.run(t, tc.user, tc.pipeline)
			if got := refused(err); got != tc.refuse {
				t.Errorf("refused = %v, want %v (err = %v)", got, tc.refuse, err)
			}
		})
	}
}

// A query with no namespace in its source spans every namespace, so it needs
// to be allowed all of them.
func TestQuery_AllNamespacesNeedsReadOnAll(t *testing.T) {
	f := newReadFixture(t)

	if err := f.run(t, f.reader, "trace"); !refused(err) {
		t.Errorf("reader of one namespace listing traces across all: err = %v, want a refusal", err)
	}
	for who, user := range map[string]string{"owner": f.owner, "system identity": ""} {
		if err := f.run(t, user, "trace"); refused(err) {
			t.Errorf("%s listing traces across all: refused: %v", who, err)
		}
	}
}
