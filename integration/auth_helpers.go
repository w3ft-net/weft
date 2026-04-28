//go:build integration

package integration

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/store"
)

// openTestStore connects to the dev postgres instance the
// docker-compose stack stands up. Migrations are already applied by
// the running coordinator; store.Open re-runs goose.Up which is a
// no-op when all migrations are current. Lives here so both the
// `!localstack` and `localstack` test partitions can use it.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(postgresURL)
	if err != nil {
		t.Fatalf("open store at %s: %v", postgresURL, err)
	}
	return db
}

// ensureToken creates (or re-uses) a token for the given customer/namespace
// via the coordinator's CreateToken RPC. Used by tests that open authed
// IngestStreams on behalf of a specific customer.
func ensureToken(t *testing.T, client pb.WeftCoordinatorClient, customer, namespace string) string {
	t.Helper()
	resp, err := client.CreateToken(context.Background(), &pb.CreateTokenRequest{
		Customer:  customer,
		Namespace: namespace,
		Label:     "integration-test",
	})
	if err != nil {
		t.Fatalf("create token for %s: %v", customer, err)
	}
	return resp.Token
}

// authedContext returns a context with an "authorization: Bearer <token>"
// metadata header, matching what weftd's PerRPCCredentials send.
func authedContext(ctx context.Context, token string) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
}

// openAuthedStream opens an IngestStream carrying a bearer token. Mirrors
// the production weftd flow — the stream's customer is resolved server-side
// from the token, so rows are attributed to that customer regardless of
// what row.Customer the test sends.
func openAuthedStream(
	t *testing.T,
	ctx context.Context,
	client pb.WeftCoordinatorClient,
	token string,
) pb.WeftCoordinator_IngestStreamClient {
	t.Helper()
	stream, err := client.IngestStream(authedContext(ctx, token))
	if err != nil {
		t.Fatalf("open ingest stream: %v", err)
	}
	return stream
}
