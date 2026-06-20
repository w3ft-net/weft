package shard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestLambdaCompactor_RoundTrip exercises the client wire: it sends a
// rewrite request to a fake server that echoes back what it received,
// proving the request serializes (op/src/dst/cutoffs) and the response
// deserializes (counts/path/stats) correctly. Creds are nil → unsigned.
func TestLambdaCompactor_RoundTrip(t *testing.T) {
	var got CompactRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(CompactResult{
			RowsKept: 3, RowsDeleted: 2, OutputPath: got.Dst,
			Stats: []CustomerStat{{Customer: "acme", RowCount: 3}},
		})
	}))
	defer srv.Close()

	c := NewLambdaCompactor(srv.URL, nil, "us-east-2")
	cutoff := time.Unix(0, 1_700_000_000*int64(time.Second)).UTC()
	res, err := c.Rewrite("s3://b/src.parquet", "s3://b/dst.parquet", []CustomerCutoff{{Customer: "acme", Cutoff: cutoff}})
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}

	// Request arrived intact.
	if got.Op != CompactOpRewrite || got.Src != "s3://b/src.parquet" || got.Dst != "s3://b/dst.parquet" {
		t.Fatalf("server saw %+v", got)
	}
	if len(got.Cutoffs) != 1 || got.Cutoffs[0].Customer != "acme" || !got.Cutoffs[0].Cutoff.Equal(cutoff) {
		t.Fatalf("cutoffs wire-tripped wrong: %+v", got.Cutoffs)
	}
	// Response parsed.
	if res.RowsKept != 3 || res.RowsDeleted != 2 || res.OutputPath != "s3://b/dst.parquet" {
		t.Fatalf("res = %+v", res)
	}
	if len(res.Stats) != 1 || res.Stats[0].Customer != "acme" {
		t.Fatalf("stats = %+v", res.Stats)
	}
}

func TestLambdaCompactor_CaptureAndErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req CompactRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Op == CompactOpCapture {
			json.NewEncoder(w).Encode(CompactResult{Stats: []CustomerStat{{Customer: "x", RowCount: 9}}})
			return
		}
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewLambdaCompactor(srv.URL, nil, "us-east-2")

	stats, err := c.CaptureStats("s3://b/x.parquet")
	if err != nil {
		t.Fatalf("CaptureStats: %v", err)
	}
	if len(stats) != 1 || stats[0].RowCount != 9 {
		t.Fatalf("stats = %+v", stats)
	}

	// A non-200 from the Lambda surfaces as an error, not a zero result.
	if _, err := c.Rewrite("s3://b/x", "s3://b/y", nil); err == nil {
		t.Fatal("expected error on 500 response")
	}
}
