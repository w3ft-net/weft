package service

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/w3ft-net/weft/backends/defaults"
	"github.com/w3ft-net/weft/backends/token"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/sqlitedb"
)

// A stream authenticates against any token.Store: here the open build's
// SQLite one, with no Postgres, in a build whose owners map to no org.
func TestAuthenticateStream_OverTheLocalTokenStore(t *testing.T) {
	db, err := sqlitedb.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sources, err := defaults.NewSourceBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := defaults.NewTokenBackend(db)
	if err != nil {
		t.Fatal(err)
	}
	ns, err := sources.FindOrCreateNamespace(shape.Added{}, "ops")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, err := tokens.CreateToken(shape.Added{}, ns.ID, "agent", "prod")
	if err != nil {
		t.Fatal(err)
	}

	s := &Server{Tokens: tokens}
	bearer := func(tok string) context.Context {
		return metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
	}
	auth, err := s.authenticateStream(bearer(raw))
	if err != nil {
		t.Fatalf("a valid token: %v", err)
	}
	if auth.Namespace != "ops" || auth.Env != "prod" || auth.TokenID == 0 {
		t.Errorf("auth = %+v, want ops/prod with a token id", auth)
	}
	if _, err := s.authenticateStream(bearer("wft_00000000000000000000000000000000")); status.Code(err) != codes.Unauthenticated {
		t.Errorf("an unknown token: %v, want Unauthenticated", err)
	}
	if err := tokens.RevokeToken(shape.Added{}, mustPublicID(t, tokens)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authenticateStream(bearer(raw)); status.Code(err) != codes.Unauthenticated {
		t.Errorf("a revoked token: %v, want Unauthenticated", err)
	}
}

// mustPublicID is the public id of the store's only token.
func mustPublicID(t *testing.T, ts interface {
	ListTokens(shape.Added) ([]token.Token, error)
}) string {
	t.Helper()
	list, err := ts.ListTokens(shape.Added{})
	if err != nil || len(list) != 1 {
		t.Fatalf("tokens = %+v, err=%v", list, err)
	}
	return list[0].PublicID
}
