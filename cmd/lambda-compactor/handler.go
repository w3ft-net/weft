package main

import (
	"encoding/json"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"net/http"

	_ "github.com/duckdb/duckdb-go/v2"

	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/tenancy/hosted/retention"
)

// HandleCompact runs one compaction op (rewrite or capture) against the
// parquet named in the request and returns the result. Unlike the
// cold-shard query handler this is a batch op — a single JSON response,
// no streaming — since the coordinator's finalizer needs the whole
// result (counts + survivor stats) before it can swap parquet_path.
// Testable without the Lambda runtime.
func HandleCompact(req retention.CompactRequest, s3cfg *shard.S3Config) (retention.CompactResult, error) {
	c := retention.LocalCompactor{S3: s3cfg, Layout: req.Layout}
	switch req.Op {
	case retention.CompactOpRewrite:
		return c.Rewrite(req.Src, req.Dst, req.Cutoffs)
	case retention.CompactOpCapture:
		stats, err := c.CaptureStats(req.Src)
		return retention.CompactResult{Stats: stats}, err
	default:
		return retention.CompactResult{}, fmt.Errorf("unknown op %q", req.Op)
	}
}

// ServeHTTP is the HTTP handler for the compactor endpoint, shared by the
// Lambda function URL handler and the test httptest.Server. Errors surface
// as a non-2xx status before any body is written, so the caller gets an
// honest failure (the rewrite either fully succeeded or it didn't —
// there's no partial-stream subtlety like the query handler has).
func ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req retention.CompactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("bad request: %v", err), http.StatusBadRequest)
		return
	}
	if req.Src == "" {
		http.Error(w, "src required", http.StatusBadRequest)
		return
	}
	if req.Op == retention.CompactOpRewrite && req.Dst == "" {
		http.Error(w, "dst required for rewrite", http.StatusBadRequest)
		return
	}

	res, err := HandleCompact(req, shard.S3ConfigFromEnv())
	if err != nil {
		http.Error(w, fmt.Sprintf("compact failed: %v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(res); err != nil {
		weftsdk.Infof(r.Context(), "lambda-compactor: encode response: %v", err)
	}
}
