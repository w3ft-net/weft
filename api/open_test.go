package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

// openIdentity is the open build's one HTTP Basic credential, as cmd/api
// supplies it; it is repeated here because a main package cannot be imported.
type openIdentity struct{ basic defaults.Basic }

func (i openIdentity) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !i.basic.Authenticate(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", `Basic realm="weft"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		RecordAccessIdentity(r, i.basic.User)
		next.ServeHTTP(w, r.WithContext(identity.WithCaller(r.Context(), i.basic.Caller())))
	})
}
func (openIdentity) Protect(next http.Handler) http.Handler  { return next }
func (openIdentity) Entitled(context.Context, Session) error { return nil }
func (openIdentity) Forward(r *http.Request) map[string]string {
	if h := r.Header.Get("Authorization"); h != "" {
		return map[string]string{"authorization": h}
	}
	return nil
}
func (openIdentity) Routes(*Routes) {}

// openAPI is an api server over the open build's stores: one SQLite database,
// the one group the default shape has, and one authenticated user. Under a
// shape with group columns the open build has no group to run as, so a test
// that builds one skips.
type openAPI struct {
	t     *testing.T
	srv   *Server
	st    *defaults.Stores
	owner shape.Added
	h     http.Handler
}

const (
	openUser     = "weft"
	openPassword = "pw"
)

func newOpenAPI(t *testing.T) *openAPI {
	t.Helper()
	if coordinator.NoGroup(shape.Added{}) {
		t.Skip("the open build has no group under a shape with required columns")
	}
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := defaults.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer("localhost:9020", "http://localhost:3000")
	srv.SetIdentity(openIdentity{basic: defaults.Basic{User: openUser, Password: openPassword}})
	srv.SetAccess(defaults.OpenAccess())
	srv.SetTokens(st.Tokens)
	srv.SetSources(st.Sources)
	srv.SetCrons(st.Crons)
	srv.SetBins(st.Bins)
	srv.SetNotify(st.Notify)
	srv.SetIngestFailures(st.Ingest)
	srv.SetMounts(&mounts.Backends{
		Sources:    st.Sources,
		BinService: binscript.Service{Bins: st.Bins, Crons: st.Crons, Sources: st.Sources},
		Notify:     st.Notify,
		CronAuthor: cronauthor.Service{Crons: st.Crons, Sources: st.Sources, Bins: st.Bins},
		Tmp:        st.Tmp,
	})
	srv.SetAlertKinds(alerting.NewRegistry(st.Notify))
	return &openAPI{t: t, srv: srv, st: st, h: srv.Handler()}
}

// credential is the Authorization header value of the open build's user.
func (a *openAPI) credential() string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(openUser+":"+openPassword))
}

// do issues a request as the open build's user and returns the response.
func (a *openAPI) do(method, path string, body []byte) *httptest.ResponseRecorder {
	a.t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Authorization", a.credential())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	a.h.ServeHTTP(w, req)
	return w
}

// namespace is the namespace name, which it creates.
func (a *openAPI) namespace(name string) uint {
	a.t.Helper()
	n, err := a.st.Sources.FindOrCreateNamespace(a.owner, name)
	if err != nil {
		a.t.Fatal(err)
	}
	return n.ID
}
