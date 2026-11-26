package apitest_test

import (
	"bytes"
	"context"
	"encoding/base64"
	shellcore "github.com/graybeardcorp/warp/pkg/shell"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/w3ft-net/weft/binscript"
	"github.com/w3ft-net/weft/cronauthor"
	"github.com/w3ft-net/weft/identity"

	"github.com/w3ft-net/weft/alerting"
	"github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/api/apitest"
	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/coordinator"
	"github.com/w3ft-net/weft/mounts"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

// openIdentity is the open build's one HTTP Basic credential. The production
// one is in cmd/api, a main package, so it cannot be imported here.
type openIdentity struct{ basic defaults.Basic }

func (i openIdentity) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !i.basic.Authenticate(r.Header.Get("Authorization")) {
			w.Header().Set("WWW-Authenticate", `Basic realm="weft"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		api.RecordAccessIdentity(r, i.basic.User)
		next.ServeHTTP(w, r.WithContext(identity.WithCaller(r.Context(), i.basic.Caller())))
	})
}
func (openIdentity) Protect(next http.Handler) http.Handler      { return next }
func (openIdentity) Entitled(context.Context, api.Session) error { return nil }
func (openIdentity) Forward(r *http.Request) map[string]string {
	if h := r.Header.Get("Authorization"); h != "" {
		return map[string]string{"authorization": h}
	}
	return nil
}
func (openIdentity) Routes(*api.Routes) {}

// openSubject is the open build's api over one SQLite database, with the one
// group the default shape has. Under a shape with group columns the open build
// has no group to run as, so the suites skip.
func openSubject(t *testing.T) apitest.Subject {
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
	basic := defaults.Basic{User: "weft", Password: "pw"}
	srv := api.NewServer("localhost:9020", "http://localhost:3000")
	srv.SetIdentity(openIdentity{basic: basic})
	srv.SetAccess(defaults.OpenAccess())
	srv.SetTokens(st.Tokens)
	srv.SetSources(st.Sources)
	srv.SetCrons(st.Crons)
	srv.SetBins(st.Bins)
	srv.SetDashboards(st.Dashboards)
	srv.SetNotify(st.Notify)
	srv.SetIngestFailures(st.Ingest)
	srv.SetMounts(&mounts.Backends{
		Sources:    st.Sources,
		BinService: binscript.Service{Bins: st.Bins, Crons: st.Crons, Sources: st.Sources},
		Notify:     st.Notify,
		CronAuthor: cronauthor.Service{Crons: st.Crons, Sources: st.Sources, Bins: st.Bins},
		Tmp:        st.Tmp,
		EtcExtensions: func(c mounts.Config) []shellcore.Mount {
			return []shellcore.Mount{mounts.Dashboards(c, st.Dashboards)}
		},
	})
	srv.SetAlertKinds(alerting.NewRegistry(st.Notify))
	h := srv.Handler()
	cred := "Basic " + base64.StdEncoding.EncodeToString([]byte(basic.User+":"+basic.Password))
	header := http.Header{}
	return apitest.Subject{
		Server: srv,
		Header: header,
		SetAuthz: func(a identity.Authorizer) {
			ad := defaults.OpenAccess()
			ad.Authz = a
			srv.SetAccess(ad)
		},
		Do: func(method, path string, body []byte) *httptest.ResponseRecorder {
			req := httptest.NewRequest(method, path, bytes.NewReader(body))
			for k, v := range header {
				req.Header[k] = v
			}
			req.Header.Set("Authorization", cred)
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			return w
		},
		Anon: func(method, path string, body []byte) *httptest.ResponseRecorder {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest(method, path, bytes.NewReader(body)))
			return w
		},
		Owner: shape.Added{}, Tokens: st.Tokens, Sources: st.Sources, Crons: st.Crons, Bins: st.Bins, Notify: st.Notify, Ingest: st.Ingest,
	}
}

func TestTokens_Open(t *testing.T)     { apitest.Tokens(t, openSubject) }
func TestCrons_Open(t *testing.T)      { apitest.Crons(t, openSubject) }
func TestIngestion_Open(t *testing.T)  { apitest.Ingestion(t, openSubject) }
func TestSources_Open(t *testing.T)    { apitest.Sources(t, openSubject) }
func TestSchema_Open(t *testing.T)     { apitest.Schema(t, openSubject) }
func TestIngest_Open(t *testing.T)     { apitest.Ingest(t, openSubject) }
func TestAuth_Open(t *testing.T)       { apitest.Auth(t, openSubject) }
func TestAuthorizer_Open(t *testing.T) { apitest.Authorizer(t, openSubject) }
func TestBins_Open(t *testing.T)       { apitest.Bins(t, openSubject) }
func TestDashboards_Open(t *testing.T) { apitest.Dashboards(t, openSubject) }
