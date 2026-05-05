package shard

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4signer "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	_ "github.com/duckdb/duckdb-go/v2"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// ColdShard implements the Shard interface for archived data in Parquet.
// Two modes: in-process (opens an in-memory DuckDB with read_parquet)
// or remote (POSTs queries to a Lambda/HTTP handler that does the same).
// Set functionURL to "" for in-process mode.
type ColdShard struct {
	id          string
	apps        []string
	parquetPath string

	// In-process mode.
	db *sql.DB

	// Remote mode (Lambda function URL or test httptest server).
	functionURL string
	client      *http.Client
	creds       aws.CredentialsProvider
	region      string
}

// LambdaQueryRequest is the JSON request body sent to a remote cold-query handler.
type LambdaQueryRequest struct {
	SQL         string `json:"sql"`
	ParquetPath string `json:"parquet_path"`
}

// NewColdShard creates a read-only shard backed by a Parquet file.
// If functionURL is non-empty, queries are sent to that HTTP endpoint
// instead of running in-process. Pass nil for s3cfg when using local
// filesystem or remote mode.
func NewColdShard(id string, apps []string, parquetPath, functionURL string, s3cfg *S3Config) (*ColdShard, error) {
	cs := &ColdShard{
		id:          id,
		apps:        apps,
		parquetPath: parquetPath,
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

func (s *ColdShard) Archive(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("cold shard %s is already archived", s.id)
}

func (s *ColdShard) readParquetExpr() string {
	if strings.Contains(s.parquetPath, "*") {
		return fmt.Sprintf("read_parquet('%s', hive_partitioning=true)", s.parquetPath)
	}
	return fmt.Sprintf("read_parquet('%s')", s.parquetPath)
}

// Query executes a SQL query against the archived Parquet data.
func (s *ColdShard) Query(query string) ([]*pb.ResultRow, error) {
	if s.functionURL != "" {
		return s.queryRemote(query)
	}
	return s.queryLocal(query)
}

func (s *ColdShard) queryLocal(query string) ([]*pb.ResultRow, error) {
	rewritten := strings.Replace(query, "FROM logs", "FROM "+s.readParquetExpr(), 1)
	sqlRows, err := s.db.Query(rewritten)
	if err != nil {
		return nil, fmt.Errorf("cold query %s: %w", s.id, err)
	}
	defer sqlRows.Close()
	return ScanResultRows(sqlRows)
}

func (s *ColdShard) queryRemote(query string) ([]*pb.ResultRow, error) {
	body, err := json.Marshal(LambdaQueryRequest{
		SQL:         query,
		ParquetPath: s.parquetPath,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, s.functionURL, bytes.NewReader(body))
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
		return nil, fmt.Errorf("lambda %s returned %d", s.id, resp.StatusCode)
	}

	return ReadAllResultRows(resp.Body)
}

// QueryToSink streams query results to a RowSink.
func (s *ColdShard) QueryToSink(query string, sink RowSink) {
	defer sink.SendSentinel(s.id)

	if s.functionURL != "" {
		s.queryToSinkRemote(query, sink)
		return
	}

	rows, err := s.queryLocal(query)
	if err != nil {
		return
	}
	for _, r := range rows {
		sink.SendRow(r, s.id)
	}
}

func (s *ColdShard) queryToSinkRemote(query string, sink RowSink) {
	body, err := json.Marshal(LambdaQueryRequest{
		SQL:         query,
		ParquetPath: s.parquetPath,
	})
	if err != nil {
		return
	}

	req, err := http.NewRequest(http.MethodPost, s.functionURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if err := s.signRequest(req, body); err != nil {
		return
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	for {
		row, err := ReadResultRow(resp.Body)
		if err != nil {
			return
		}
		sink.SendRow(row, s.id)
	}
}

// Ingest always returns an error — cold shards are read-only.
func (s *ColdShard) Ingest(rows []*pb.LogRow) error {
	return fmt.Errorf("cold shard %s is read-only", s.id)
}

func (s *ColdShard) signRequest(req *http.Request, body []byte) error {
	if s.creds == nil {
		return nil
	}
	hash := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(hash[:])
	creds, err := s.creds.Retrieve(req.Context())
	if err != nil {
		return fmt.Errorf("retrieve credentials: %w", err)
	}
	signer := v4signer.NewSigner()
	return signer.SignHTTP(req.Context(), creds, req, payloadHash, "lambda", s.region, time.Now())
}

// Close releases the in-memory DuckDB connection (no-op in remote mode).
func (s *ColdShard) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}
