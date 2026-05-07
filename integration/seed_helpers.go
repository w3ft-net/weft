//go:build integration

package integration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kevlarcon/weft/auth"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

// devJWTSecret is the JWT signing key the docker-compose stack uses.
// Aligned with the compiled-in default in cmd/coordinator/main.go and
// the WEFT_TEST_JWT_SECRET override here for parity with integrationJWT.
func devJWTSecret() string {
	if s := os.Getenv("WEFT_TEST_JWT_SECRET"); s != "" {
		return s
	}
	return "dev-secret-change-in-production"
}

// customerJWT signs a JWT for the named customer. The auth interceptor
// validates the signature and resolves the user via BuildSession, so
// the user row must exist (see seedCustomerWithNamespace). Used by tests
// that need per-tenant identity (cross-tenant isolation, multi-customer
// flows) where the global integrationJWT (always "kevlar") collapses
// every caller to one tenant.
func customerJWT(t *testing.T, customer string) string {
	t.Helper()
	now := time.Now()
	tok, err := auth.Generate(&store.User{
		ID:              fmt.Sprintf("test-user-%s", customer),
		Email:           fmt.Sprintf("%s@test.local", customer),
		EmailVerifiedAt: &now,
		CreatedAt:       now,
	}, devJWTSecret())
	if err != nil {
		t.Fatalf("sign JWT for %s: %v", customer, err)
	}
	return tok
}

// grpcClientAs returns a coordinator gRPC client whose outbound calls
// carry a JWT signed for the named customer. Closing the returned conn
// is the caller's responsibility (defer conn.Close()).
func grpcClientAs(t *testing.T, customer string) (*grpc.ClientConn, pb.WeftCoordinatorClient) {
	t.Helper()
	tok := customerJWT(t, customer)
	conn, err := grpc.NewClient(CoordAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(auth.ClientTokenUnaryInterceptor(tok)),
		grpc.WithStreamInterceptor(auth.ClientTokenStreamInterceptor(tok)),
	)
	if err != nil {
		t.Fatalf("connect to coordinator at %s: %v", CoordAddr, err)
	}
	return conn, pb.NewWeftCoordinatorClient(conn)
}

// seedCustomerWithNamespace upserts a customer row, a namespace, a user,
// and a team_members row directly via postgres. Returns (customerID,
// namespaceID). The user ID matches what customerJWT embeds
// ("test-user-<customer>") so BuildSession can resolve the JWT into a
// session with the correct customer.
func seedCustomerWithNamespace(t *testing.T, customer, namespace string) (int64, int64) {
	t.Helper()
	conn, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer conn.Close()

	var custID int64
	if err := conn.QueryRow(`
		INSERT INTO customers (name, created_at, updated_at)
		VALUES ($1, now(), now())
		ON CONFLICT (name) DO UPDATE SET updated_at = now()
		RETURNING id
	`, customer).Scan(&custID); err != nil {
		t.Fatalf("seed customer %s: %v", customer, err)
	}

	var nsID int64
	if err := conn.QueryRow(`
		INSERT INTO namespaces (customer_id, name, created_at, updated_at)
		VALUES ($1, $2, now(), now())
		ON CONFLICT (customer_id, name) DO UPDATE SET updated_at = now()
		RETURNING id
	`, custID, namespace).Scan(&nsID); err != nil {
		t.Fatalf("seed namespace %s/%s: %v", customer, namespace, err)
	}

	userID := fmt.Sprintf("test-user-%s", customer)
	email := fmt.Sprintf("%s@test.local", customer)
	if _, err := conn.Exec(`
		INSERT INTO users (id, email, email_verified_at, created_at, updated_at)
		VALUES ($1, $2, now(), now(), now())
		ON CONFLICT (id) DO NOTHING
	`, userID, email); err != nil {
		t.Fatalf("seed user for %s: %v", customer, err)
	}
	if _, err := conn.Exec(`
		INSERT INTO team_members (id, customer_id, email, user_id, role, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'owner', 'active', now(), now())
		ON CONFLICT DO NOTHING
	`, fmt.Sprintf("tm-%s", customer), custID, email, userID); err != nil {
		t.Fatalf("seed team_member for %s: %v", customer, err)
	}

	return custID, nsID
}

// seedAgentToken inserts an agent token row directly into postgres for
// (customer, namespace) and returns the raw token string the agent
// would present. Mirrors store.CreateAgentToken's hashing scheme so
// store.ValidateAgentToken accepts the result.
//
// The customer + namespace must already exist (call
// seedCustomerWithNamespace first). This bypasses CreateToken's
// JWT-derived customer constraint, which is what makes the
// ghost-customer test possible: we inject a token whose customer is
// then deleted out from under it.
func seedAgentToken(t *testing.T, custID, nsID int64, label string) string {
	t.Helper()
	raw, err := generateRawAgentToken()
	if err != nil {
		t.Fatalf("generate agent token: %v", err)
	}
	hash := sha256.Sum256([]byte(raw))
	tokenHash := hex.EncodeToString(hash[:])
	prefix := raw[:8]

	conn, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Exec(`
		INSERT INTO agent_tokens (
			customer_id, namespace_id, token_hash, prefix, label,
			revoked, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, false, now(), now())
	`, custID, nsID, tokenHash, prefix, label); err != nil {
		t.Fatalf("seed agent token: %v", err)
	}
	return raw
}

// generateRawAgentToken mirrors store.generateToken: 16 random bytes
// hex-encoded, prefixed with `wft_`. Kept separate from the store
// package so the harness owns its preconditions without exporting
// store internals.
func generateRawAgentToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("wft_%x", b[:]), nil
}

// deleteCustomer DROPs the customer row by name. The cascade migration
// (006) takes care of children — agent tokens, namespaces, log groups,
// and so on. Used to set up the deleted-customer ingest-stream test.
func deleteCustomer(t *testing.T, name string) {
	t.Helper()
	conn, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(),
		`DELETE FROM customers WHERE name = $1`, name); err != nil {
		t.Fatalf("delete customer %s: %v", name, err)
	}
}
