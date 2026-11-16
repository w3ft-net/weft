// Package apitest is the behaviour suites of the api's REST routes that both
// builds must show: the open one over SQLite stores and the hosted one over
// Postgres. A suite reaches data through the contracts and requests through the
// handler, as the build's authenticated owner, so it never names a store, a user
// or a credential. A build's own test supplies a Subject and calls the suite, as
// coordtest does for the coordinator.
package apitest

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/w3ft-net/weft/api"
	"github.com/w3ft-net/weft/backends/bin"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/backends/ingestfail"
	"github.com/w3ft-net/weft/backends/source"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/notifycfg"
	"github.com/w3ft-net/weft/shape"
)

// Subject is a fresh api server, the stores behind it, and the callers a suite
// acts as. The owner is the group's one authenticated user, who may do
// everything.
type Subject struct {
	Server *api.Server
	// Header is added to every request Do makes; a suite sets what it needs.
	Header http.Header
	// Do requests as the owner.
	Do func(method, path string, body []byte) *httptest.ResponseRecorder
	// Anon requests with no credential.
	Anon func(method, path string, body []byte) *httptest.ResponseRecorder
	// DoOther requests as the owner of another group, and Other is that group:
	// both nil/zero for a build with one group, where the suites that need a
	// second skip.
	DoOther func(method, path string, body []byte) *httptest.ResponseRecorder
	Other   shape.Added

	Owner   shape.Added
	Tokens  token.Store
	Sources source.Store
	Crons   cron.Store
	Bins    bin.Store
	Notify  notifycfg.Service
	Ingest  ingestfail.Store
	// SetAuthz replaces the build's Authorizer with the given one.
	SetAuthz func(identity.Authorizer)
	// RecordAgentSeen notes that an agent of that version read the source, for a
	// build that tracks the agents of each source; nil when it does not.
	RecordAgentSeen func(t *testing.T, sourceID uint, instance, version string)
}

// needsOther skips a test that keeps one group's data from another's.
func (s Subject) needsOther(t *testing.T) {
	t.Helper()
	if s.DoOther == nil {
		t.Skip("this build has one group to keep apart from another")
	}
}

// namespace is the owner's namespace of that name, which it creates.
func (s Subject) namespace(t *testing.T, name string) uint {
	t.Helper()
	n, err := s.Sources.FindOrCreateNamespace(s.Owner, name)
	if err != nil {
		t.Fatal(err)
	}
	return n.ID
}

// post sends v as JSON and returns the response.
func (s Subject) post(t *testing.T, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return s.Do("POST", path, b)
}

// decode reads a response body into v.
func decode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(bytes.NewReader(w.Body.Bytes())).Decode(v); err != nil {
		t.Fatalf("decode %q: %v", w.Body.String(), err)
	}
}

// each runs fn for every case as a subtest on a fresh Subject.
func each(t *testing.T, newSubject func(t *testing.T) Subject, cases []namedCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.run(t, newSubject(t)) })
	}
}

type namedCase struct {
	name string
	run  func(*testing.T, Subject)
}
