// Package tokenstoretest is the conformance suite for token.Store: the
// behaviour every implementation of the agent-token domain (Postgres for the
// hosted build, SQLite for the open one) must show. An implementation's test
// calls Run with a constructor.
package tokenstoretest

import (
	"errors"
	"reflect"
	"regexp"
	"testing"

	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/shape"
)

// Subject is one store under test, fresh and empty.
type Subject struct {
	Store token.Store
	// Owner owns the tokens the suite mints. The open build's is the empty value.
	Owner shape.Added
	// Other is a second owner, for a store that has owners to tell apart. Nil for
	// one that has none.
	Other *shape.Added
	// Namespace makes a namespace for the owner (or for Other, when owner is it)
	// that tokens can be minted for. The namespaces are another domain's, so the
	// subject makes one in that domain's terms, in the same database.
	Namespace func(t *testing.T, owner shape.Added, name string) token.NamespaceRef
	// DeleteNamespace deletes a namespace made by Namespace, as the sources
	// domain does.
	DeleteNamespace func(t *testing.T, owner shape.Added, name string)
}

// Run runs the suite, building a fresh Subject for each test.
func Run(t *testing.T, newSubject func(t *testing.T) Subject) {
	cases := []struct {
		name string
		fn   func(*testing.T, Subject)
	}{
		{"CreateToken", createToken},
		{"ValidateToken", validateToken},
		{"RevokeToken", revokeToken},
		{"TokensBelongToTheirOwner", tokenOwners},
		{"ListTokens", listTokens},
		{"CountLiveTokens", countLiveTokens},
		{"DeletingANamespaceDeletesItsTokens", deletingANamespace},
		{"SetMetricsConfig", setMetricsConfig},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newSubject(t)) })
	}
}

var (
	rawForm    = regexp.MustCompile(`^wft_[0-9a-f]{32}$`)
	publicForm = regexp.MustCompile(`^tok_[0-9a-f]{16}$`)
)

func mint(t *testing.T, s Subject, ns token.NamespaceRef, label, env string) (string, *token.Token) {
	t.Helper()
	raw, tok, err := s.Store.CreateToken(s.Owner, ns.ID, label, env)
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	return raw, tok
}

func createToken(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	raw, tok := mint(t, s, ns, "prod-coordinator", "prod")
	if !rawForm.MatchString(raw) {
		t.Fatalf("raw token %q is not wft_<32 hex>", raw)
	}
	if !publicForm.MatchString(tok.PublicID) || tok.Prefix != raw[:8] || tok.Label != "prod-coordinator" || tok.Env != "prod" ||
		tok.Revoked || tok.Namespace != ns || tok.ID == 0 || tok.CreatedAt.IsZero() {
		t.Fatalf("token: %+v for %q", tok, raw)
	}
	raw2, tok2 := mint(t, s, ns, "", "")
	if raw2 == raw || tok2.PublicID == tok.PublicID {
		t.Fatal("two tokens shared a secret or a public id")
	}
	if tok2.Env != "" || tok2.Label != "" {
		t.Fatalf("optional fields: %+v", tok2)
	}
	// A token is for a namespace that exists.
	if _, tok, err := s.Store.CreateToken(s.Owner, ns.ID+1000, "", ""); err == nil {
		t.Fatalf("minted a token for a namespace that does not exist: %+v", tok)
	}
}

func validateToken(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	raw, tok := mint(t, s, ns, "l", "staging")

	auth, err := s.Store.ValidateToken(raw)
	if err != nil || auth == nil {
		t.Fatalf("ValidateToken: %+v err=%v", auth, err)
	}
	if auth.Namespace != "ops" || auth.Env != "staging" || auth.TokenID != tok.ID || auth.Owner != s.Owner {
		t.Fatalf("auth: %+v, want ops/staging for token %d owned by %+v", auth, tok.ID, s.Owner)
	}
	for _, bad := range []string{"", "wft_" + "0123456789abcdef0123456789abcdef", raw + "x", raw[:len(raw)-1]} {
		if got, err := s.Store.ValidateToken(bad); got != nil || err != nil {
			t.Errorf("ValidateToken(%q) = %+v, err=%v; want nil, nil", bad, got, err)
		}
	}
}

func revokeToken(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	raw, tok := mint(t, s, ns, "l", "")
	if err := s.Store.RevokeToken(s.Owner, tok.PublicID); err != nil {
		t.Fatal(err)
	}
	if auth, _ := s.Store.ValidateToken(raw); auth != nil {
		t.Fatal("a revoked token still validates")
	}
	// Revoking again is fine; revoking one that does not exist is not.
	if err := s.Store.RevokeToken(s.Owner, tok.PublicID); err != nil {
		t.Fatalf("revoking twice: %v", err)
	}
	if err := s.Store.RevokeToken(s.Owner, "tok_0000000000000000"); !errors.Is(err, token.ErrNotFound) {
		t.Fatalf("revoking an absent token: %v, want ErrNotFound", err)
	}
}

func tokenOwners(t *testing.T, s Subject) {
	if s.Other == nil {
		t.Skip("the store has no owners to tell apart")
	}
	ns := s.Namespace(t, s.Owner, "ops")
	_, mine := mint(t, s, ns, "mine", "")
	theirNS := s.Namespace(t, *s.Other, "ops")
	rawTheirs, theirs, err := s.Store.CreateToken(*s.Other, theirNS.ID, "theirs", "")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Store.RevokeToken(*s.Other, mine.PublicID); !errors.Is(err, token.ErrNotFound) {
		t.Fatalf("revoking another owner's token: %v, want ErrNotFound", err)
	}
	if auth, _ := s.Store.ValidateToken(rawTheirs); auth == nil || auth.Owner != *s.Other {
		t.Fatalf("the other owner's token validates as %+v", auth)
	}
	list, _ := s.Store.ListTokens(*s.Other)
	if len(list) != 1 || list[0].PublicID != theirs.PublicID {
		t.Fatalf("the other owner lists %+v", list)
	}
	if n, _ := s.Store.CountLiveTokens(*s.Other, ns.ID); n != 0 {
		t.Fatalf("an owner counted another's namespace's tokens: %d", n)
	}
}

func listTokens(t *testing.T, s Subject) {
	ops := s.Namespace(t, s.Owner, "ops")
	app := s.Namespace(t, s.Owner, "app")
	_, a := mint(t, s, ops, "a", "")
	_, b := mint(t, s, app, "b", "prod")
	if err := s.Store.RevokeToken(s.Owner, b.PublicID); err != nil {
		t.Fatal(err)
	}

	list, err := s.Store.ListTokens(s.Owner)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListTokens: %+v err=%v, want both tokens, revoked included", list, err)
	}
	byID := map[string]token.Token{}
	for _, tk := range list {
		byID[tk.PublicID] = tk
	}
	if got := byID[a.PublicID]; got.Namespace != ops || got.Label != "a" || got.Revoked {
		t.Errorf("token a listed as %+v", got)
	}
	if got := byID[b.PublicID]; got.Namespace != app || got.Env != "prod" || !got.Revoked {
		t.Errorf("token b listed as %+v", got)
	}
}

func countLiveTokens(t *testing.T, s Subject) {
	ops := s.Namespace(t, s.Owner, "ops")
	app := s.Namespace(t, s.Owner, "app")
	_, a := mint(t, s, ops, "a", "")
	mint(t, s, ops, "b", "")
	mint(t, s, app, "c", "")
	if n, err := s.Store.CountLiveTokens(s.Owner, ops.ID); err != nil || n != 2 {
		t.Fatalf("live tokens of ops: %d err=%v, want 2", n, err)
	}
	if err := s.Store.RevokeToken(s.Owner, a.PublicID); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Store.CountLiveTokens(s.Owner, ops.ID); n != 1 {
		t.Fatalf("after a revoke: %d, want 1", n)
	}
	if n, _ := s.Store.CountLiveTokens(s.Owner, app.ID); n != 1 {
		t.Fatalf("another namespace's count moved: %d", n)
	}
}

// The namespaces are the sources domain's, but a token does not outlive its
// namespace: deleting one takes its tokens, and no other namespace's.
func deletingANamespace(t *testing.T, s Subject) {
	ops := s.Namespace(t, s.Owner, "ops")
	app := s.Namespace(t, s.Owner, "app")
	raw, _ := mint(t, s, ops, "a", "")
	rawKeep, _ := mint(t, s, app, "b", "")
	s.DeleteNamespace(t, s.Owner, "ops")
	if auth, _ := s.Store.ValidateToken(raw); auth != nil {
		t.Fatal("a deleted namespace's token still validates")
	}
	if auth, _ := s.Store.ValidateToken(rawKeep); auth == nil {
		t.Fatal("another namespace's token was deleted")
	}
	list, _ := s.Store.ListTokens(s.Owner)
	if len(list) != 1 || list[0].Namespace != app {
		t.Fatalf("after the delete: %+v", list)
	}
	if n, err := s.Store.CountLiveTokens(s.Owner, ops.ID); err != nil || n != 0 {
		t.Fatalf("live tokens of the deleted namespace: %d err=%v", n, err)
	}
}

func setMetricsConfig(t *testing.T, s Subject) {
	ns := s.Namespace(t, s.Owner, "ops")
	raw, tok := mint(t, s, ns, "a", "")
	cfg := token.MetricsConfig{Enabled: true, Interval: "15s", Collectors: []string{"cpu", "memory"}}
	if err := s.Store.SetMetricsConfig(raw, cfg); err != nil {
		t.Fatal(err)
	}
	list, _ := s.Store.ListTokens(s.Owner)
	var got token.Token
	for _, tk := range list {
		if tk.PublicID == tok.PublicID {
			got = tk
		}
	}
	if !got.MetricsEnabled || got.MetricsInterval != "15s" || !reflect.DeepEqual(got.MetricsCollectors, []string{"cpu", "memory"}) {
		t.Fatalf("metrics config: %+v", got)
	}
	// Turning it off clears the collectors.
	if err := s.Store.SetMetricsConfig(raw, token.MetricsConfig{}); err != nil {
		t.Fatal(err)
	}
	list, _ = s.Store.ListTokens(s.Owner)
	if got := list[0]; got.MetricsEnabled || len(got.MetricsCollectors) != 0 {
		t.Fatalf("after turning it off: %+v", got)
	}
	// A token nobody has is a no-op.
	if err := s.Store.SetMetricsConfig("wft_00000000000000000000000000000000", cfg); err != nil {
		t.Fatalf("an unknown token: %v", err)
	}
}
