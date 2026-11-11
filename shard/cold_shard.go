package shard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4signer "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	_ "github.com/duckdb/duckdb-go/v2"

	weftsdk "github.com/w3ft-net/weft-go"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

// ColdShard implements the Shard interface for archived data in Parquet.
// Two modes: in-process (opens an in-memory DuckDB with read_parquet)
// or remote (POSTs queries to a Lambda/HTTP handler that does the same).
// Set functionURL to "" for in-process mode.
type ColdShard struct {
	id          string
	apps        []string
	parquetPath string
	// layout is the partition layout the archive was written in.
	layout shape.Layout

	// In-process mode.
	db *sql.DB

	// Remote mode (Lambda function URL or test httptest server).
	functionURL string
	client      *http.Client
	creds       aws.CredentialsProvider
	region      string
}

// LambdaQueryRequest is the JSON request body sent to a remote cold-query handler.
//
// Args carries positional placeholder values for `?` markers in SQL.
// Omitted (omitempty) for legacy/in-process callers that still use
// string-interpolated SQL — the lambda handler tolerates empty args.
// Strings only today; see ExecuteQueryRequest.args for the rationale.
type LambdaQueryRequest struct {
	SQL         string   `json:"sql"`
	ParquetPath string   `json:"parquet_path"`
	Args        []string `json:"args,omitempty"`
	// TraceId carries the ambient trace ID (if any) explicitly — the
	// Lambda is a separate AWS-managed process that can't read a Go
	// context.Context, so this is the only channel. Keep in sync with
	// cmd/lambda-shard/handler.go's QueryRequest.
	TraceId string `json:"trace_id,omitempty"`
	// DeadlineUnixMs is when the caller stops waiting (zero: no deadline).
	// The Lambda interrupts the query there rather than billing on after
	// the coordinator has hung up. Keep in sync with
	// cmd/lambda-shard/handler.go's QueryRequest.
	DeadlineUnixMs int64 `json:"deadline_unix_ms,omitempty"`
	// Layout is the partition layout the archive was written in, which the
	// handler needs to type the archive's hive columns. Absent from an older
	// caller: the handler then reads the legacy layout.
	Layout shape.Layout `json:"layout,omitzero"`
}

// ColdOption configures NewColdShard.
type ColdOption func(*ColdShard)

// ColdLayout sets the partition layout the archive was written in. A shard made
// without it reads the legacy layout; so does one given a zero layout.
func ColdLayout(l shape.Layout) ColdOption {
	return func(s *ColdShard) {
		if !l.IsZero() {
			s.layout = l
		}
	}
}

// deadlineUnixMs is ctx's deadline for the wire, or zero if it has none.
func deadlineUnixMs(ctx context.Context) int64 {
	if d, ok := ctx.Deadline(); ok {
		return d.UnixMilli()
	}
	return 0
}

// anyArgsToStrings narrows planner-supplied []any to []string for the
// wire format. The planner produces strings only (customer id,
// ISO-formatted timestamps, namespace/env/service names); non-string
// values fall through as fmt.Sprintf("%v", a) and surface in shard
// logs as a typed-bug breadcrumb.
func anyArgsToStrings(args []any) []string {
	if len(args) == 0 {
		return nil
	}
	out := make([]string, len(args))
	for i, a := range args {
		if s, ok := a.(string); ok {
			out[i] = s
		} else {
			out[i] = fmt.Sprintf("%v", a)
		}
	}
	return out
}

// NewColdShard creates a read-only shard backed by a Parquet file.
// If functionURL is non-empty, queries are sent to that HTTP endpoint
// instead of running in-process. Pass nil for s3cfg when using local
// filesystem or remote mode.
func NewColdShard(id string, apps []string, parquetPath, functionURL string, s3cfg *S3Config, opts ...ColdOption) (*ColdShard, error) {
	cs := &ColdShard{
		id:          id,
		apps:        apps,
		parquetPath: parquetPath,
		layout:      shape.Legacy(),
	}
	for _, o := range opts {
		o(cs)
	}

	if functionURL != "" {
		cs.functionURL = functionURL
		cs.client = &http.Client{Timeout: 5 * time.Minute}
		return cs, nil
	}

	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open in-memory duckdb: %w", err)
	}
	if s3cfg != nil {
		if err := s3cfg.ConfigureDuckDB(db); err != nil {
			db.Close()
			return nil, err
		}
	}
	cs.db = db
	return cs, nil
}

// WithAWSAuth enables SigV4 request signing for Lambda function URL
// calls. Required when the function URL uses AWS_IAM authorization.
// Returns the receiver for chaining.
func (s *ColdShard) WithAWSAuth(creds aws.CredentialsProvider, region string) *ColdShard {
	s.creds = creds
	s.region = region
	return s
}

func (s *ColdShard) ID() string { return s.id }

func (s *ColdShard) Archive(_ context.Context, _ string) (string, []CustomerStat, error) {
	return "", nil, fmt.Errorf("cold shard %s is already archived", s.id)
}

func (s *ColdShard) readParquetExpr() string { return ParquetReadExpr(s.parquetPath, s.layout) }

// hiveTypes pins the columns a keyed archive keeps in its directory names: the
// layout's partition columns, typed as it declares them, then the key. Left to
// DuckDB, a column whose values all look like numbers (every service named
// like a number) is typed as one, and a predicate on a name that isn't a
// number then fails to bind. The time column _part is left to be typed as a
// timestamp, which PruneColdPartitions relies on. A sized archive adds the
// key's bucket width, which it keeps in a directory of its own.
func hiveTypes(layout shape.Layout, sized bool) string {
	var parts []string
	for _, c := range layout.Columns {
		parts = append(parts, fmt.Sprintf("'%s': '%s'", c.Name, c.Type.SQL()))
	}
	parts = append(parts, "'namespace': 'VARCHAR'", "'service': 'VARCHAR'", "'kind': 'VARCHAR'")
	if sized {
		parts = append(parts, "'_w': 'BIGINT'")
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// ParquetReadExpr builds the DuckDB read_parquet(...) source for a stored
// path. A glob (the hive-partitioned layout from a large shard's archive)
// enables hive_partitioning so the partition columns are read back, and for a
// keyed archive types the key columns per the layout the archive was written
// in; a plain path reads a single file. The cold-shard Lambda builds the same
// expression.
func ParquetReadExpr(path string, layout shape.Layout) string {
	if !strings.Contains(path, "*") {
		return fmt.Sprintf("read_parquet('%s')", path)
	}
	if IsSized(path) {
		return fmt.Sprintf("read_parquet('%s', hive_partitioning=true, hive_types=%s)", path, hiveTypes(layout, true))
	}
	if IsKeyed(path) {
		return fmt.Sprintf("read_parquet('%s', hive_partitioning=true, hive_types=%s)", path, hiveTypes(layout, false))
	}
	return fmt.Sprintf("read_parquet('%s', hive_partitioning=true)", path)
}

// Query executes a SQL query against the archived Parquet data.
// args carries positional placeholder values for `?` markers in query.
func (s *ColdShard) Query(ctx context.Context, query string, args ...any) ([]*pb.ResultRow, error) {
	if s.functionURL != "" {
		return s.queryRemote(ctx, query, args)
	}
	return s.queryLocal(ctx, query, args)
}

func (s *ColdShard) queryLocal(ctx context.Context, query string, args []any) ([]*pb.ResultRow, error) {
	rewritten := strings.Replace(query, "FROM logs", "FROM "+s.readParquetExpr(), 1)
	sqlRows, err := s.db.QueryContext(ctx, rewritten, args...)
	if err != nil {
		return nil, fmt.Errorf("cold query %s: %w", s.id, err)
	}
	defer sqlRows.Close()
	return ScanResultRows(sqlRows)
}

func (s *ColdShard) queryRemote(ctx context.Context, query string, args []any) ([]*pb.ResultRow, error) {
	traceID, _ := weftsdk.TraceIDFromContext(ctx)
	body, err := json.Marshal(LambdaQueryRequest{
		SQL:            query,
		ParquetPath:    s.parquetPath,
		Args:           anyArgsToStrings(args),
		TraceId:        traceID,
		DeadlineUnixMs: deadlineUnixMs(ctx),
		Layout:         s.layout,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.functionURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := s.signRequest(req, body); err != nil {
		return nil, fmt.Errorf("sign request for %s: %w", s.id, err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call lambda %s: %w", s.id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("lambda %s returned %d: %s", s.id, resp.StatusCode, body)
	}

	return ReadAllResultRows(resp.Body)
}

// QueryToSink streams query results to a RowSink.
func (s *ColdShard) QueryToSink(ctx context.Context, query string, args []any, sink RowSink) {
	defer sink.SendSentinel(s.id)

	if s.functionURL != "" {
		if err := s.queryToSinkRemote(ctx, query, args, sink); err != nil {
			sink.Fail(s.id, err)
		}
		return
	}

	rows, err := s.queryLocal(ctx, query, args)
	if err != nil {
		sink.Fail(s.id, err)
		return
	}
	for _, r := range rows {
		sink.SendRow(r, s.id)
	}
}

// queryToSinkRemote streams the Lambda's rows to sink; a non-nil error
// means the rows it sent (if any) are not all of them.
func (s *ColdShard) queryToSinkRemote(ctx context.Context, query string, args []any, sink RowSink) error {
	start := time.Now()
	traceID, _ := weftsdk.TraceIDFromContext(ctx)

	body, err := json.Marshal(LambdaQueryRequest{
		SQL:            query,
		ParquetPath:    s.parquetPath,
		Args:           anyArgsToStrings(args),
		TraceId:        traceID,
		DeadlineUnixMs: deadlineUnixMs(ctx),
		Layout:         s.layout,
	})
	if err != nil {
		weftsdk.Infof(ctx, "cold shard %s: marshal request: %v", s.id, err)
		return fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.functionURL, bytes.NewReader(body))
	if err != nil {
		weftsdk.Infof(ctx, "cold shard %s: build request: %v", s.id, err)
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if err := s.signRequest(req, body); err != nil {
		weftsdk.Infof(ctx, "cold shard %s: sign request: %v", s.id, err)
		return fmt.Errorf("sign request: %w", err)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		weftsdk.Errorf(ctx, "cold shard %s: lambda call failed: %v", s.id, err)
		return fmt.Errorf("lambda call failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		weftsdk.Infof(ctx, "cold shard %s: lambda returned %d: %s", s.id, resp.StatusCode, body)
		return fmt.Errorf("lambda returned %d: %s", resp.StatusCode, body)
	}

	rowsSeen := 0
	for {
		row, err := ReadResultRow(resp.Body)
		if err == io.EOF {
			// Unlike a live shard (which stays silent unless actively
			// traced), a cold-shard query always logs completion —
			// archived-data queries are the rarer, costlier path (the
			// one most likely to explain hidden latency, per the
			// incident that originated this feature), so unconditional
			// logging here doesn't reintroduce the log-spam problem a
			// hot live-shard path would have.
			// The trailer is optional: a Lambda from before it ends at the
			// sentinel, and a malformed one costs only this log field.
			if stats, ok, _ := ReadScanStats(resp.Body); ok {
				if r, isReporter := sink.(ScanReporter); isReporter {
					r.ReportScan(s.id, stats)
				}
				weftsdk.Infof(ctx, "cold shard %s: query completed in %s (%d rows, rows_scanned=%d bytes_read=%d)", s.id, time.Since(start), rowsSeen, stats.RowsScanned, stats.BytesRead)
			} else {
				weftsdk.Infof(ctx, "cold shard %s: query completed in %s (%d rows)", s.id, time.Since(start), rowsSeen)
			}
			return nil
		}
		if err != nil {
			weftsdk.Errorf(ctx, "cold shard %s: stream read error after %d rows: %v", s.id, rowsSeen, err)
			return fmt.Errorf("stream read error after %d rows: %w", rowsSeen, err)
		}
		rowsSeen++
		sink.SendRow(row, s.id)
	}
}

// Ingest always returns an error — cold shards are read-only. The
// recorder is ignored: a cold shard in the ingest path is a routing
// bug, not a flaky-shard failure, so it shouldn't trip the
// suspend counter.
func (s *ColdShard) Ingest(_ context.Context, rows []*pb.LogRow, _ IngestRecorder) error {
	return fmt.Errorf("cold shard %s is read-only", s.id)
}

// IngestBatch is refused for the same reason as Ingest.
func (s *ColdShard) IngestBatch(ctx context.Context, b Batch, rec IngestRecorder) error {
	return s.Ingest(ctx, b.Rows, rec)
}

func (s *ColdShard) signRequest(req *http.Request, body []byte) error {
	return SignLambdaV4(req, body, s.creds, s.region)
}

// SignLambdaV4 SigV4-signs a Lambda Function URL request when creds are
// present (the URL uses AWS_IAM auth); a nil provider is a no-op for the
// unsigned test/localstack path. Shared by the cold-shard query client
// and the compactor client.
func SignLambdaV4(req *http.Request, body []byte, creds aws.CredentialsProvider, region string) error {
	if creds == nil {
		return nil
	}
	hash := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(hash[:])
	c, err := creds.Retrieve(req.Context())
	if err != nil {
		return fmt.Errorf("retrieve credentials: %w", err)
	}
	return v4signer.NewSigner().SignHTTP(req.Context(), c, req, payloadHash, "lambda", region, time.Now())
}

// Close releases the in-memory DuckDB connection (no-op in remote mode).
func (s *ColdShard) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
