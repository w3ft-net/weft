package service

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/w3ft-net/weft/logtable"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// A per-key shard behind its real gRPC server: rows ingested over the wire land
// in a table per key, a query naming the table reads one, and a query that
// names none (an operator tool, an older coordinator) reads them all.
func TestPerKeyShard_OverTheWire(t *testing.T) {
	dir := t.TempDir()
	if err := shard.SaveIdentity(dir, &shard.Identity{ShardID: "wire-test"}); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServerFromIdentity(dir, filepath.Join(dir, "shard.duckdb"), WithDriver(shard.DuckDBDriver(shard.WithPerKeyTables())))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shard().Close()
	c := serve(t, srv)
	ctx := context.Background()

	var rows []*pb.LogRow
	for i := int64(0); i < 3; i++ {
		rows = append(rows,
			&pb.LogRow{Raw: "api run=7 ok", Namespace: "acme", Service: "api", Kind: "main", CustomerId: 5, TsUnixNs: (1_700_000_000 + i) * 1e9},
			&pb.LogRow{Raw: "web run=7 ok", Namespace: "acme", Service: "web", Kind: "main", CustomerId: 5, TsUnixNs: (1_700_000_000 + i) * 1e9},
		)
	}
	if _, err := c.Ingest(ctx, &pb.IngestRequest{Rows: rows}); err != nil {
		t.Fatal(err)
	}
	if !srv.Shard().(*shard.DuckDBShard).PerKey() {
		t.Fatal("the shard is not per-key")
	}

	query := func(req *pb.QueryRequest) []*pb.ResultRow {
		t.Helper()
		stream, err := c.Query(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		var out []*pb.ResultRow
		for {
			r, err := stream.Recv()
			if err == io.EOF {
				return out
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
	}

	api := logtable.Name([]any{int64(5)}, "acme", "api", "main")
	if got := query(&pb.QueryRequest{Sql: "SELECT raw, ts FROM logs WHERE customer_id = ?", Args: []string{"5"}, Table: api}); len(got) != 3 {
		t.Errorf("hinted query returned %d rows, want 3", len(got))
	}
	if got := query(&pb.QueryRequest{Sql: "SELECT raw, ts FROM logs WHERE customer_id = ?", Args: []string{"5"}}); len(got) != 6 {
		t.Errorf("unhinted query returned %d rows, want 6", len(got))
	}
	// weft-admin shard-count asks this with no table.
	got := query(&pb.QueryRequest{
		Sql: `SELECT CAST(count(*) AS VARCHAR) AS raw, date_trunc('second', ts) AS ts
		 FROM logs WHERE service = ? AND raw LIKE ? GROUP BY 2 ORDER BY 2`,
		Args: []string{"web", "%run=7 %"},
	})
	if len(got) != 3 {
		t.Fatalf("shard-count's query returned %d seconds, want 3", len(got))
	}
	for _, r := range got {
		if r.Raw != "1" {
			t.Errorf("a second counted %q rows, want 1", r.Raw)
		}
	}
}
