package service

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shard"
)

// The layout fields are additive, so a coordinator and a shard on different
// sides of the change must still agree. docs/plans/tenant-scope.md, "Wire
// transition". An older coordinator is one that does not send them at all. No
// older shard is left: one built before layouts reported its archive stats by
// customer_id alone, a field that is now reserved.

func serveBehind(t *testing.T, srv *Server) pb.ShardQueryClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	gs := grpc.NewServer(grpc.ChainUnaryInterceptor(srv.UnaryInterceptor()), grpc.ChainStreamInterceptor(srv.StreamInterceptor()))
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

// Every pairing of old and new coordinator, for ingest and archive: the rows
// land under customer 42, and the archive reports customer 42's rows to the
// coordinator.
func TestCrossVersion_IngestAndArchiveAgree(t *testing.T) {
	const customer = 42
	for _, tc := range []struct {
		name string
		// oldCoordinator never sends the layout or the partition values.
		oldCoordinator bool
	}{
		{"new coordinator, new shard", false},
		{"old coordinator, new shard", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			srv := NewBlankServer(dir, filepath.Join(dir, "shard.duckdb"))
			c := serveBehind(t, srv)
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
			stats := shard.PartitionStatsFromProto(arch.CustomerStats)
			if len(stats) != 1 || !reflect.DeepEqual(stats[0].PartitionValues, []string{"42"}) || stats[0].RowCount != 2 {
				t.Fatalf("archive stats = %+v, want customer 42 with 2 rows", stats)
			}
		})
	}
}

// When the two disagree the batch's partition values win: a coordinator that
// has stopped stamping tag 26 must not be read as customer 0.
func TestCrossVersion_BatchValuesWinOverTag26(t *testing.T) {
	dir := t.TempDir()
	srv := NewBlankServer(dir, filepath.Join(dir, "shard.duckdb"))
	c := serveBehind(t, srv)
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
