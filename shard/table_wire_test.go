package shard

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// The table a plan names travels to the shard beside sql
// (docs/plans/per-service-tables.md). QueryToSink and QueryFieldsToSink leave
// it off, so a query that names no table is byte-for-byte what it was.
func TestGRPCShard_QueryTableToSinkSendsTable(t *testing.T) {
	srv := &reqCaptureServer{}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	pb.RegisterShardQueryServer(gs, srv)
	go gs.Serve(lis)
	defer gs.GracefulStop()

	sh, err := NewGRPCShard("table-shard", lis.Addr().String(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()

	var _ TableQuerier = sh
	ctx := context.Background()
	sh.QueryTableToSink(ctx, "l_abc", "SELECT plain", "SELECT fields", []any{"a"}, discardSink{})
	sh.QueryFieldsToSink(ctx, "SELECT plain", "SELECT fields", []any{"a"}, discardSink{})
	sh.QueryToSink(ctx, "SELECT plain", []any{"a"}, discardSink{})

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.reqs) != 3 {
		t.Fatalf("%d queries, want 3", len(srv.reqs))
	}
	if r := srv.reqs[0]; r.Table != "l_abc" || r.Sql != "SELECT plain" || r.FieldSql != "SELECT fields" || len(r.Args) != 1 {
		t.Errorf("QueryTableToSink sent %+v", r)
	}
	for i, name := range []string{"QueryFieldsToSink", "QueryToSink"} {
		if r := srv.reqs[i+1]; r.Table != "" {
			t.Errorf("%s sent table %q, want none", name, r.Table)
		}
	}
}

// A shard in the per-key layout reads the named table; the same call with
// field SQL runs the field form against it.
func TestDuckDBShard_QueryTableToSinkReadsTheNamedTable(t *testing.T) {
	s := openPerKey(t, t.TempDir()+"/s.duckdb")
	ingestAll(t, s)

	for _, c := range []struct {
		name          string
		sql, fieldSQL string
	}{
		{"plain", "SELECT raw, ts FROM logs", ""},
		{"field form", "SELECT 'unused' AS raw, ts FROM logs", "SELECT raw, ts FROM logs"},
	} {
		sink := &collectSink{}
		s.QueryTableToSink(context.Background(), pkStats.table(), c.sql, c.fieldSQL, nil, sink)
		if sink.err != nil {
			t.Fatalf("%s: %v", c.name, sink.err)
		}
		if len(sink.rows) != 5 {
			t.Errorf("%s: %d rows, want 5 (the stats table's)", c.name, len(sink.rows))
		}
	}
}

type collectSink struct {
	rows []*pb.ResultRow
	err  error
}

func (c *collectSink) SendRow(r *pb.ResultRow, _ string) { c.rows = append(c.rows, r) }
func (c *collectSink) Fail(_ string, err error)          { c.err = err }
func (c *collectSink) SendSentinel(string)               {}
