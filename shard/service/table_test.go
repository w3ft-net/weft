package service

import (
	"context"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"

	"github.com/graybeardcorp/weft/logtable"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

type rowCollectStream struct {
	grpc.ServerStream
	ctx  context.Context
	rows []*pb.ResultRow
}

func (s *rowCollectStream) Context() context.Context { return s.ctx }
func (s *rowCollectStream) Send(r *pb.ResultRow) error {
	s.rows = append(s.rows, r)
	return nil
}

// A query that names a table reads only that table on a shard with a table
// per log source; field_sql still runs in place of sql against it.
// docs/plans/per-service-tables.md.
func TestQuery_ReadsTheTableTheRequestNames(t *testing.T) {
	dir := t.TempDir()
	ds, err := shard.NewDuckDBShard("table-test", filepath.Join(dir, "s.duckdb"), shard.WithPerKeyTables())
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	var rows []*pb.LogRow
	for i := 0; i < 3; i++ {
		rows = append(rows,
			&pb.LogRow{Raw: "api line", Namespace: "acme", Service: "api", Kind: "main", CustomerId: 5, TsUnixNs: int64(1_700_000_000+i) * 1e9},
			&pb.LogRow{Raw: "web line", Namespace: "acme", Service: "web", Kind: "main", CustomerId: 5, TsUnixNs: int64(1_700_000_000+i) * 1e9},
		)
	}
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}
	srv := NewBlankServer(dir, filepath.Join(dir, "s.duckdb"))
	srv.shard = ds

	apiTable := logtable.Name(5, "acme", "api", "main")
	for _, c := range []struct {
		name string
		req  *pb.QueryRequest
		want int
		raw  string
	}{
		{"named table", &pb.QueryRequest{Sql: "SELECT raw, ts FROM logs", Table: apiTable}, 3, "api line"},
		{"field form against it", &pb.QueryRequest{Sql: "SELECT 'x' AS raw, ts FROM logs", FieldSql: "SELECT raw, ts FROM logs", Table: apiTable}, 3, "api line"},
		{"no table reads every row", &pb.QueryRequest{Sql: "SELECT raw, ts FROM logs"}, 6, ""},
	} {
		stream := &rowCollectStream{ctx: context.Background()}
		if err := srv.Query(c.req, stream); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(stream.rows) != c.want {
			t.Errorf("%s: %d rows, want %d", c.name, len(stream.rows), c.want)
		}
		if c.raw != "" {
			for _, r := range stream.rows {
				if r.Raw != c.raw {
					t.Errorf("%s: row %q from the wrong table", c.name, r.Raw)
				}
			}
		}
	}
}
