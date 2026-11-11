package service

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
	"github.com/graybeardcorp/weft/shard"
)

// The layout fields are additive, so a coordinator and a shard on different
// sides of the change must still agree. docs/plans/tenant-scope.md, "Wire
// transition". A shard from before the change drops the fields it has no
// number for and never sends the ones it has no name for, which is what
// oldShardInterceptor does to the real shard; an older coordinator is one that
// does not send them at all.

// oldShardInterceptor makes the shard behind it behave as one built before
// layouts: Configure.layout and Ingest.partition_values never arrive, and the
// archive stats it returns have no partition_values.
func oldShardInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	switch r := req.(type) {
	case *pb.ConfigureRequest:
		r.Layout = nil
	case *pb.IngestRequest:
		r.PartitionValues = nil
	}
	resp, err := handler(ctx, req)
	if a, ok := resp.(*pb.ArchiveResponse); ok {
		for _, s := range a.CustomerStats {
			s.PartitionValues = nil
		}
	}
	return resp, err
}

func serveBehind(t *testing.T, srv *Server, pre grpc.UnaryServerInterceptor) pb.ShardQueryClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	unary := []grpc.UnaryServerInterceptor{srv.UnaryInterceptor()}
	if pre != nil {
		unary = append([]grpc.UnaryServerInterceptor{pre}, unary...)
	}
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(unary...), grpc.ChainStreamInterceptor(srv.StreamInterceptor()))
	pb.RegisterShardQueryServer(gs, srv)
	go gs.Serve(lis)
	t.Cleanup(gs.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return lis.Dial() }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewShardQueryClient(conn)
}

func countRows(t *testing.T, c pb.ShardQueryClient, query string) int {
	t.Helper()
	stream, err := c.Query(context.Background(), &pb.QueryRequest{Sql: query})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		if _, err := stream.Recv(); err == io.EOF {
			return n
		} else if err != nil {
			t.Fatal(err)
		}
		n++
	}
}

// Every pairing of old and new, for ingest and archive: the rows land under
// customer 42, and the archive reports customer 42's rows to the coordinator.
func TestCrossVersion_IngestAndArchiveAgree(t *testing.T) {
	const customer = 42
	for _, tc := range []struct {
		name string
		// oldShard drops the new fields on arrival; oldCoordinator never sends
		// them.
		oldShard, oldCoordinator bool
	}{
		{"new coordinator, new shard", false, false},
		{"new coordinator, old shard", true, false},
		{"old coordinator, new shard", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srv := NewBlankServer(dir, filepath.Join(dir, "shard.duckdb"))
			var pre grpc.UnaryServerInterceptor
			if tc.oldShard {
				pre = oldShardInterceptor
			}
			c := serveBehind(t, srv, pre)
			ctx := context.Background()

			cfg := &pb.ConfigureRequest{ShardId: "s1"}
			batch := &pb.IngestRequest{Rows: []*pb.LogRow{
				{Raw: "a b c", TsUnixNs: 1_700_000_000 * 1e9, Namespace: "ns", Service: "svc", CustomerId: customer},
				{Raw: "d e f", TsUnixNs: 1_700_000_001 * 1e9, Namespace: "ns", Service: "svc", CustomerId: customer},
			}}
			if !tc.oldCoordinator {
				cfg.Layout = shard.LayoutToProto(shape.Legacy())
				batch.PartitionValues = []string{"42"}
			}
			if _, err := c.Configure(ctx, cfg); err != nil {
				t.Fatalf("Configure: %v", err)
			}
			if _, err := c.Ingest(ctx, batch); err != nil {
				t.Fatalf("Ingest: %v", err)
			}
			if n := countRows(t, c, "SELECT raw, ts FROM logs WHERE customer_id = 42"); n != 2 {
				t.Fatalf("rows stored under customer 42 = %d, want 2", n)
			}

			arch, err := c.Archive(ctx, &pb.ArchiveRequest{S3Prefix: t.TempDir()})
			if err != nil {
				t.Fatalf("Archive: %v", err)
			}
			stats := shard.CustomerStatsFromProto(arch.CustomerStats)
			if len(stats) != 1 || stats[0].CustomerID != customer || stats[0].RowCount != 2 {
				t.Fatalf("archive stats = %+v, want customer 42 with 2 rows", stats)
			}
			// What a coordinator can rely on either way is CustomerID; a new
			// shard also says it positionally.
			if hasValues := len(stats[0].PartitionValues) > 0; hasValues == tc.oldShard {
				t.Errorf("partition values present = %v with oldShard = %v", hasValues, tc.oldShard)
			}
		})
	}
}

// When the two disagree the batch's partition values win: a coordinator that
// has stopped stamping tag 26 must not be read as customer 0.
func TestCrossVersion_BatchValuesWinOverTag26(t *testing.T) {
	dir := t.TempDir()
	srv := NewBlankServer(dir, filepath.Join(dir, "shard.duckdb"))
	c := serveBehind(t, srv, nil)
	ctx := context.Background()
	if _, err := c.Configure(ctx, &pb.ConfigureRequest{ShardId: "s1", Layout: shard.LayoutToProto(shape.Legacy())}); err != nil {
		t.Fatal(err)
	}
	// No CustomerId on the row at all: the cut-over state.
	rows := []*pb.LogRow{{Raw: "x", TsUnixNs: 1_700_000_000 * 1e9, Namespace: "ns", Service: "svc"}}
	if _, err := c.Ingest(ctx, &pb.IngestRequest{Rows: rows, PartitionValues: []string{"42"}}); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, c, "SELECT raw, ts FROM logs WHERE customer_id = 42"); n != 1 {
		t.Fatalf("rows under customer 42 = %d, want 1", n)
	}
}
