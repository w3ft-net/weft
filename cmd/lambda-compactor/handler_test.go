package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// testParquet ingests rows and archives them to a local temp dir.
func testParquet(t *testing.T, rows []*pb.LogRow) string {
	t.Helper()
	ds, err := shard.NewDuckDBShard("compact-lambda-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()
	if err := ds.Ingest(rows, nil); err != nil {
		t.Fatal(err)
	}
	path, _, err := ds.Archive(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHandleCompact_Rewrite(t *testing.T) {
	base := int64(1_600_000_000) * int64(time.Second)
	src := testParquet(t, []*pb.LogRow{
		{Raw: "old", CustomerId: 5, TsUnixNs: base},
		{Raw: "new", CustomerId: 5, TsUnixNs: base + 100*int64(24*time.Hour)},
	})
	dst := t.TempDir() + "/out.parquet"

	res, err := HandleCompact(shard.CompactRequest{
		Op:  shard.CompactOpRewrite,
		Src: src,
		Dst: dst,
		Cutoffs: []shard.CustomerCutoff{
			{CustomerID: 5, Cutoff: time.Unix(0, base+50*int64(24*time.Hour))},
		},
	}, nil)
	if err != nil {
		t.Fatalf("HandleCompact: %v", err)
	}
	if res.RowsKept != 1 || res.RowsDeleted != 1 || res.OutputPath != dst {
		t.Fatalf("res = %+v, want kept 1 / deleted 1 / dst %q", res, dst)
	}
	if len(res.Stats) != 1 || res.Stats[0].CustomerID != 5 || res.Stats[0].RowCount != 1 {
		t.Errorf("survivor stats = %+v, want acme/1", res.Stats)
	}
}

func TestHandleCompact_Capture(t *testing.T) {
	base := int64(1_600_000_000) * int64(time.Second)
	src := testParquet(t, []*pb.LogRow{
		{Raw: "a", CustomerId: 5, TsUnixNs: base},
		{Raw: "b", CustomerId: 6, TsUnixNs: base},
	})

	res, err := HandleCompact(shard.CompactRequest{Op: shard.CompactOpCapture, Src: src}, nil)
	if err != nil {
		t.Fatalf("HandleCompact: %v", err)
	}
	if len(res.Stats) != 2 {
		t.Fatalf("capture stats = %+v, want 2 customers", res.Stats)
	}
}

func TestServeHTTP_RewriteRoundTrip(t *testing.T) {
	base := int64(1_600_000_000) * int64(time.Second)
	src := testParquet(t, []*pb.LogRow{{Raw: "new", CustomerId: 5, TsUnixNs: base + 100*int64(24*time.Hour)}})
	dst := t.TempDir() + "/out.parquet"

	srv := httptest.NewServer(http.HandlerFunc(ServeHTTP))
	defer srv.Close()

	body, _ := json.Marshal(shard.CompactRequest{Op: shard.CompactOpRewrite, Src: src, Dst: dst})
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var res shard.CompactResult
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res.RowsKept != 1 || res.OutputPath != dst {
		t.Errorf("res = %+v, want kept 1 / dst %q", res, dst)
	}
}

func TestServeHTTP_Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(ServeHTTP))
	defer srv.Close()

	cases := []struct {
		name string
		req  shard.CompactRequest
		want int
	}{
		{"missing src", shard.CompactRequest{Op: shard.CompactOpCapture}, http.StatusBadRequest},
		{"rewrite missing dst", shard.CompactRequest{Op: shard.CompactOpRewrite, Src: "x"}, http.StatusBadRequest},
		{"missing parquet", shard.CompactRequest{Op: shard.CompactOpCapture, Src: "/nope/missing.parquet"}, http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(tc.req)
			resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}
