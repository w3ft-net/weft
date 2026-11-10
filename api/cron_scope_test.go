package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/graybeardcorp/weft/store"
)

// A cron homed in a namespace is listed and fetched only for those who can
// read the namespace; everyone else gets the answer for a cron that isn't there.
func TestRESTCrons_NamespaceReadersOnly(t *testing.T) {
	g := newGroupedOrg(t, store.TierYard)
	hidden, err := g.s.FindOrCreateNamespace(g.org.ID, "hidden")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		ns   uint
		name string
	}{{hidden.ID, "secret"}, {g.ops, "visible"}, {0, "shared"}} {
		if err := g.s.SaveCronWatch(g.org.ID, c.ns, "", c.name, "*/5 * * * *", "echo 1", true, nil); err != nil {
			t.Fatal(err)
		}
	}

	names := func(jwt string) []string {
		w := g.do(jwt, "GET", "/api/crons", nil)
		if w.Code != 200 {
			t.Fatalf("list: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			Crons []struct {
				Name, Path string
			}
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, c := range out.Crons {
			got = append(got, c.Path)
		}
		return got
	}
	if got := names(g.plain); len(got) != 2 || got[0] == "hidden/secret" || got[1] == "hidden/secret" {
		t.Errorf("member lists %v, want ops/visible and shared only", got)
	}
	if got := names(g.owner); len(got) != 3 {
		t.Errorf("owner lists %v, want all three", got)
	}

	for path, want := range map[string]int{
		"/api/crons/hidden/secret":         http.StatusNotFound,
		"/api/crons/hidden/secret/results": http.StatusNotFound,
		"/api/crons/ops/visible":           http.StatusOK,
		"/api/crons/shared":                http.StatusOK,
	} {
		if w := g.do(g.plain, "GET", path, nil); w.Code != want {
			t.Errorf("member GET %s = %d, want %d", path, w.Code, want)
		}
	}
	if w := g.do(g.owner, "GET", "/api/crons/hidden/secret", nil); w.Code != http.StatusOK {
		t.Errorf("owner GET hidden/secret = %d", w.Code)
	}
}
