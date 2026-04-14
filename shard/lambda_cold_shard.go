package shard

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// LambdaColdShard implements the Shard interface by calling a Lambda function
// (or HTTP endpoint) that queries a Parquet file via DuckDB read_parquet().
// The response is a stream of length-prefixed protobuf LogRow messages.
type LambdaColdShard struct {
	id          string
	apps        []string
	functionURL string
	parquetPath string
	client      *http.Client
}

// LambdaQueryRequest is the JSON request body sent to the Lambda.
type LambdaQueryRequest struct {
	SQL         string `json:"sql"`
	ParquetPath string `json:"parquet_path"`
}

// NewLambdaColdShard creates a cold shard backed by a Lambda function URL.
func NewLambdaColdShard(id string, apps []string, functionURL, parquetPath string) *LambdaColdShard {
	return &LambdaColdShard{
		id:          id,
		apps:        apps,
		functionURL: functionURL,
		parquetPath: parquetPath,
		client:      &http.Client{Timeout: 5 * time.Minute},
	}
}

func (s *LambdaColdShard) ID() string { return s.id }

// Query calls the Lambda with the SQL and Parquet path, reads the
// streaming protobuf response, and returns all rows.
func (s *LambdaColdShard) Query(sql string) ([]*pb.LogRow, error) {
	body, err := json.Marshal(LambdaQueryRequest{
		SQL:         sql,
		ParquetPath: s.parquetPath,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	resp, err := s.client.Post(s.functionURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("call lambda %s: %w", s.id, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("lambda %s returned %d", s.id, resp.StatusCode)
	}

	return ReadAllLogRows(resp.Body)
}

// QueryToSink calls the Lambda and streams rows directly to the sink.
func (s *LambdaColdShard) QueryToSink(sql string, sink RowSink) {
	defer sink.SendSentinel(s.id)

	body, err := json.Marshal(LambdaQueryRequest{
		SQL:         sql,
		ParquetPath: s.parquetPath,
	})
	if err != nil {
		return
	}

	resp, err := s.client.Post(s.functionURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return
	}

	for {
		row, err := ReadLogRow(resp.Body)
		if err != nil {
			return // EOF or error
		}
		sink.SendRow(row, s.id)
	}
}

// Ingest always returns an error — Lambda cold shards are read-only.
func (s *LambdaColdShard) Ingest(rows []*pb.LogRow) error {
	return fmt.Errorf("lambda cold shard %s is read-only", s.id)
}

// Close is a no-op for Lambda shards.
func (s *LambdaColdShard) Close() error {
	return nil
}
