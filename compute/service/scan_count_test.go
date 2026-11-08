package service

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// scanningShard answers one row and, like a cold shard, reports what it scanned.
type scanningShard struct {
	shard.Shard
	id      string
	scanned int64
}

func (s scanningShard) QueryToSink(_ context.Context, _ string, _ []any, sink shard.RowSink) {
	sink.SendRow(&pb.ResultRow{Raw: "line"}, s.id)
	if r, ok := sink.(shard.ScanReporter); ok && s.scanned > 0 {
		r.ReportScan(s.id, shard.ScanStats{RowsScanned: s.scanned})
	}
	sink.SendSentinel(s.id)
}

type scanningResolver struct{ sh scanningShard }

func (r scanningResolver) ShardID() string            { return r.sh.id }
func (r scanningResolver) Dial() (shard.Shard, error) { return r.sh, nil }

func scanCount(t *testing.T, shards ...scanningShard) (lines int, scanned int64) {
	t.Helper()
	engine := &compute.Engine{}
	var descs []*pb.ShardDescriptor
	for _, sh := range shards {
		engine.RegisterResolver(scanningResolver{sh})
		descs = append(descs, &pb.ShardDescriptor{ShardId: sh.id})
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterWeftComputeServer(srv, NewServer(engine))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	client, err := compute.NewClient(lis.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	stream, err := client.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{Sql: "SELECT 1", Shards: descs})
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		lines++
	}
	sc, ok := stream.(compute.ScanCounter)
	if !ok {
		t.Fatal("the client's stream does not report scans")
	}
	return lines, sc.RowsScanned()
}

// A cold shard's scan crosses compute's wire, summed over shards, after the lines.
func TestExecuteQueryStream_CarriesTheColdScanCount(t *testing.T) {
	lines, scanned := scanCount(t,
		scanningShard{id: "cold-a", scanned: 1_000},
		scanningShard{id: "cold-b", scanned: 234},
		scanningShard{id: "hot-c"})
	if lines != 3 || scanned != 1_234 {
		t.Errorf("lines=%d scanned=%d, want 3 and 1234", lines, scanned)
	}
}

// A query over hot shards only scans nothing cold.
func TestExecuteQueryStream_HotOnlyScansNothing(t *testing.T) {
	lines, scanned := scanCount(t, scanningShard{id: "hot-only"})
	if lines != 1 || scanned != 0 {
		t.Errorf("lines=%d scanned=%d, want 1 and 0", lines, scanned)
	}
}
