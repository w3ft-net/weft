package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/graybeardcorp/saaskit/orgs"
	"github.com/graybeardcorp/weft/store"
)

// The REST endpoints that write /bin, /cron.d and /etc must enforce the same
// group checks as the shell: binmgr, cronmgr and configmgr. They used to write
// straight to the store, so any member could change them with their own JWT.

type groupedOrg struct {
	srv *Server
	s   *store.Store
	org *store.Organization

	owner, plain, bin, cron, etc string // JWTs: the org owner / no groups / exactly one group each
	ops                          uint   // namespace "ops", readable by every seat
}

func newGroupedOrg(t *testing.T, tier store.Tier) *groupedOrg {
	t.Helper()
	srv, s := testServer(t)
	org, err := s.FindOrCreateOrg(fmt.Sprintf("restperm-%d", os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	// Every real org has a subscription row; signup creates a trial one.
	if err := s.CreateSubscription(&store.Subscription{
		ID: uuid.New().String(), OrgID: org.ID, Tier: string(tier), Status: "active", StartsAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_, ownerJWT := permsSeed(t, s, org, store.RoleOwner, "restperm-owner")

	ops, err := s.FindOrCreateNamespace(org.ID, "ops")
	if err != nil {
		t.Fatal(err)
	}
	g := &groupedOrg{srv: srv, s: s, org: org, owner: ownerJWT, ops: ops.ID}
	grant := map[string]store.TeamPermsDoc{}
	seat := func(suffix string, p store.MemberPerms) string {
		u, jwt := permsSeed(t, s, org, orgs.RoleMember, "restperm-"+suffix)
		// Every seat reads ops, so only a missing group can refuse a write there.
		p.Namespaces = map[string]store.NamespaceAcl{"ops": {Read: true}}
		grant[suffix] = store.TeamPermsDoc{u.Email: p}
		return jwt
	}
	g.plain = seat("plain", store.MemberPerms{})
	g.bin = seat("bin", store.MemberPerms{Bin: store.AccessEdit})
	g.cron = seat("cron", store.MemberPerms{Cron: store.AccessEdit})
	g.etc = seat("etc", store.MemberPerms{Etc: store.AccessEdit})

	all := store.TeamPermsDoc{}
	for _, doc := range grant {
		for email, p := range doc {
			all[email] = p
		}
	}
	if w := g.do(ownerJWT, "PUT", "/api/permissions", mustJSON(t, all)); w.Code != 204 {
		t.Fatalf("grant permissions: %d %s", w.Code, w.Body.String())
	}
	return g
}

func (g *groupedOrg) do(jwt, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.srv.Handler().ServeHTTP(w, req)
	return w
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// restWrite is one REST mutation and how to see whether it landed.
type restWrite struct {
	name   string
	method string
	path   string
	body   string
	// seed, if set, creates what the write depends on (a dashboard needs its cron).
	seed func(*groupedOrg)
	// group names which of groupedOrg's JWTs may do this.
	group func(*groupedOrg) string
	// exists reports whether the write's effect is present.
	exists func(*groupedOrg) bool
}

const permDashboard = "title: Perm\nsections:\n  - title: S\n    tiles:\n      - kind: sparkline\n        cron: permcron\n        size: md\n"

func restWrites() []restWrite {
	return []restWrite{
		{
			name: "save function", method: "PUT", path: "/api/functions/permfn", body: `{"source":"echo hi"}`,
			group:  func(g *groupedOrg) string { return g.bin },
			exists: func(g *groupedOrg) bool { _, err := g.s.GetShellFunction(g.org.ID, "permfn"); return err == nil },
		},
		{
			name: "save cron", method: "PUT", path: "/api/crons/ops/permcron", body: `{"schedule":"*/5 * * * *","pipeline":"echo 1"}`,
			group:  func(g *groupedOrg) string { return g.cron },
			exists: func(g *groupedOrg) bool { _, err := g.s.GetCronWatch(g.org.ID, g.ops, "permcron"); return err == nil },
		},
		{
			name: "save dashboard", method: "PUT", path: "/api/dashboards/permdash", body: `{"yaml_body":` + jsonString(permDashboard) + `}`,
			seed: func(g *groupedOrg) {
				g.s.SaveCronWatch(g.org.ID, 0, "", "permcron", "*/5 * * * *", "echo 1", true, nil)
			},
			group:  func(g *groupedOrg) string { return g.etc },
			exists: func(g *groupedOrg) bool { _, err := g.s.GetDashboard(g.org.ID, "permdash"); return err == nil },
		},
		{
			name: "save notify config", method: "PUT", path: "/api/notify", body: `{"aliases_yaml":"system:\n  email:\n    - \"permtest@example.test\"\n"}`,
			group: func(g *groupedOrg) string { return g.etc },
			exists: func(g *groupedOrg) bool {
				nc, err := g.s.GetNotifyConfig(g.org.ID)
				return err == nil && nc.AliasesYAML == "system:\n  email:\n    - \"permtest@example.test\"\n"
			},
		},
	}
}

func TestRESTWrites_RequireTheGroupTheShellRequires(t *testing.T) {
	for _, tc := range restWrites() {
		t.Run(tc.name, func(t *testing.T) {
			g := newGroupedOrg(t, store.TierYard)
			if tc.seed != nil {
				tc.seed(g)
			}
			groups := map[string]string{"no group": g.plain, "bin": g.bin, "cron": g.cron, "etc": g.etc}
			right := tc.group(g)

			for who, jwt := range groups {
				if jwt == right {
					continue
				}
				w := g.do(jwt, tc.method, tc.path, []byte(tc.body))
				if w.Code != http.StatusForbidden {
					t.Errorf("member with %s: status %d, want 403: %s", who, w.Code, w.Body.String())
				}
				if tc.exists(g) {
					t.Errorf("member with %s: the write landed anyway", who)
				}
			}

			w := g.do(right, tc.method, tc.path, []byte(tc.body))
			if w.Code >= 300 {
				t.Fatalf("member with the right group: status %d: %s", w.Code, w.Body.String())
			}
			if !tc.exists(g) {
				t.Error("member with the right group: the write did not land")
			}
		})
	}
}

func TestRESTDeletes_RequireTheGroupTheShellRequires(t *testing.T) {
	deletes := []struct {
		name, path string
		seed       func(*groupedOrg)
		gone       func(*groupedOrg) bool
		group      func(*groupedOrg) string
	}{
		{
			name: "delete function", path: "/api/functions/permfn",
			seed:  func(g *groupedOrg) { g.s.SaveShellFunction(g.org.ID, "", "permfn", "echo hi", "bash") },
			gone:  func(g *groupedOrg) bool { _, err := g.s.GetShellFunction(g.org.ID, "permfn"); return err != nil },
			group: func(g *groupedOrg) string { return g.bin },
		},
		{
			name: "delete cron", path: "/api/crons/ops/permcron",
			seed: func(g *groupedOrg) {
				g.s.SaveCronWatch(g.org.ID, g.ops, "", "permcron", "*/5 * * * *", "echo 1", true, nil)
			},
			gone:  func(g *groupedOrg) bool { _, err := g.s.GetCronWatch(g.org.ID, g.ops, "permcron"); return err != nil },
			group: func(g *groupedOrg) string { return g.cron },
		},
		{
			name: "delete dashboard", path: "/api/dashboards/permdash",
			seed: func(g *groupedOrg) {
				g.s.SaveCronWatch(g.org.ID, 0, "", "permcron", "*/5 * * * *", "echo 1", true, nil)
				g.s.SaveDashboard(g.org.ID, "", "permdash", permDashboard)
			},
			gone:  func(g *groupedOrg) bool { _, err := g.s.GetDashboard(g.org.ID, "permdash"); return err != nil },
			group: func(g *groupedOrg) string { return g.etc },
		},
	}
	for _, tc := range deletes {
		t.Run(tc.name, func(t *testing.T) {
			g := newGroupedOrg(t, store.TierYard)
			tc.seed(g)
			right := tc.group(g)

			for who, jwt := range map[string]string{"no group": g.plain, "bin": g.bin, "cron": g.cron, "etc": g.etc} {
				if jwt == right {
					continue
				}
				w := g.do(jwt, "DELETE", tc.path, nil)
				if w.Code != http.StatusForbidden {
					t.Errorf("member with %s: status %d, want 403: %s", who, w.Code, w.Body.String())
				}
				if tc.gone(g) {
					t.Errorf("member with %s: the delete landed anyway", who)
				}
			}

			w := g.do(right, "DELETE", tc.path, nil)
			if w.Code >= 300 {
				t.Fatalf("member with the right group: status %d: %s", w.Code, w.Body.String())
			}
			if !tc.gone(g) {
				t.Error("member with the right group: the delete did not land")
			}
		})
	}
}

// A cron saved over REST is held to the plan's limits exactly as one saved in
// the shell. The trial plan allows one cron, so the second is refused. This
// guards behavior the REST handler already had, through the move onto the
// shared write path.
func TestRESTCronSave_HonorsPlanLimitsLikeTheShell(t *testing.T) {
	g := newGroupedOrg(t, store.TierTrial)
	save := func(name string) *httptest.ResponseRecorder {
		return g.do(g.cron, "PUT", "/api/crons/ops/"+name, []byte(`{"schedule":"*/5 * * * *","pipeline":"echo 1"}`))
	}

	if w := save("first"); w.Code >= 300 {
		t.Fatalf("first cron on the trial plan: status %d: %s", w.Code, w.Body.String())
	}
	w := save("second")

	if w.Code != http.StatusForbidden {
		t.Errorf("second cron on the trial plan: status %d, want 403: %s", w.Code, w.Body.String())
	}
	if _, err := g.s.GetCronWatch(g.org.ID, g.ops, "second"); err == nil {
		t.Error("cron saved past the trial plan's limit")
	}
}

// A schedule shorter than the plan's minimum interval is a plan limit too.
func TestRESTCronSave_HonorsMinimumIntervalLikeTheShell(t *testing.T) {
	g := newGroupedOrg(t, store.TierTrial)

	w := g.do(g.cron, "PUT", "/api/crons/ops/fast", []byte(`{"schedule":"* * * * *","pipeline":"echo 1"}`))

	if w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403 for an interval below the plan minimum: %s", w.Code, w.Body.String())
	}
	if _, err := g.s.GetCronWatch(g.org.ID, g.ops, "fast"); err == nil {
		t.Error("cron saved below the plan's minimum interval")
	}
}
