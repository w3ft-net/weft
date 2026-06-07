//go:build integration && !localstack

package integration

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// TestIngestStream_RejectsDeletedCustomer guards the stream-auth fix that
// closed the production bug seen in the coordinator logs:
//
//	ingest-stream: first row from customer=kevlar app=xylem
//
// The customer "kevlar" had already been deleted from the database when
// that line was logged. The underlying cause was that IngestStream did
// not validate the caller's token or the claimed customer; it just
// trusted whatever `row.Customer` the weftd sent.
//
// After the fix: IngestStream requires an `authorization: Bearer <token>`
// metadata header, and the FK cascade on `customers` drops the token row
// alongside the customer. So a weftd holding a token for a deleted
// customer can no longer authenticate — the stream is rejected before any
// row is accepted.
//
// CreateToken now derives the customer from the caller's JWT (rather
// than accepting it as a request field), so this test seeds the ghost
// customer + namespace + agent token directly in postgres, mirroring
// what `kevcons /bin/save` would have produced for a real agent.
func TestIngestStream_RejectsDeletedCustomer(t *testing.T) {
	ctx := context.Background()

	conn, client := grpcClient(t)
	defer conn.Close()

	custName := fmt.Sprintf("ghost-%d", time.Now().UnixNano())

	// ── 1. Seed customer + namespace + agent token directly. The
	// hash scheme matches store.CreateAgentToken so the coordinator's
	// ValidateAgentToken accepts the raw value.
	custID, nsID := seedCustomerWithNamespace(t, custName, custName+"-ns")
	ghostToken := seedAgentToken(t, custID, nsID, "ghost-agent")

	// Sanity: the token validates BEFORE we delete the customer. If
	// this fails the seeding scheme is wrong, not the validation.
	if _, err := client.IngestStream(authedContext(ctx, ghostToken)); err != nil {
		t.Fatalf("precondition: token rejected before customer delete: %v", err)
	}

	// ── 2. Delete the customer directly from Postgres — mirrors what
	// happened in prod when kevlar was removed. The FK cascade
	// migration is applied by the harness, so a single DELETE on the
	// parent row drops the agent token alongside it.
	deleteCustomer(t, custName)

	// Verify the customer row really is gone (paranoia, but cheap).
	dbConn, err := sql.Open("pgx", postgresURL)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer dbConn.Close()
	var remaining int
	if err := dbConn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM customers WHERE name = $1`, custName,
	).Scan(&remaining); err != nil {
		t.Fatalf("count customers: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("precondition: customer %q still exists after delete", custName)
	}

	// ── 3. Open an IngestStream with the token the customer once held.
	// The FK cascade from step 2 dropped the token row along with the
	// customer, so validation must reject this stream.
	stream, err := client.IngestStream(authedContext(ctx, ghostToken))
	if err != nil {
		t.Fatalf("open ingest stream: %v", err)
	}
	const nRows = 5
	for i := 0; i < nRows; i++ {
		// Some of these Sends may fail once the server rejects the
		// stream — that's fine, the assertion is on the final
		// CloseAndRecv.
		_ = stream.Send(&pb.LogRow{
			Namespace: "integration", Service: "ghost",
			Customer: custName,
			Raw:      fmt.Sprintf("ghost row %d — customer no longer exists", i),
			Instance: "test-host",
			TsUnixNs: time.Now().Add(time.Duration(i) * time.Millisecond).UnixNano(),
		})
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		t.Logf("stream rejected (good): %v", err)
		return
	}

	t.Fatalf(
		"BUG: coordinator accepted %d rows for customer %q whose token "+
			"was invalidated by cascade-delete before the stream opened. "+
			"IngestStream is not enforcing token validation.",
		resp.RowsReceived, custName,
	)
}
