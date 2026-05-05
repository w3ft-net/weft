package shard

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/duckdb/duckdb-go/v2"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// ColdShard implements the Shard interface by querying a Parquet file
// via DuckDB's read_parquet(). Used for archived shards whose data has
// been exported from a live DuckDB to Parquet (locally or on S3).
type ColdShard struct {
	id          string
	apps        []string
	parquetPath string
	db          *sql.DB // throwaway in-memory DuckDB for read_parquet()
}

// NewColdShard creates a read-only shard backed by a Parquet file.
// Pass nil for s3cfg to read from local filesystem.
func NewColdShard(id string, apps []string, parquetPath string, s3cfg ...*S3Config) (*ColdShard, error) {
	db, err := sql.Open("duckdb", "")
	if err != nil {
		return nil, fmt.Errorf("open in-memory duckdb: %w", err)
	}

	if len(s3cfg) > 0 && s3cfg[0] != nil {
		if err := s3cfg[0].ConfigureDuckDB(db); err != nil {
			db.Close()
			return nil, err
		}
	}

	return &ColdShard{
		id:          id,
		apps:        apps,
		parquetPath: parquetPath,
		db:          db,
	}, nil
}

func (s *ColdShard) ID() string { return s.id }

func (s *ColdShard) Archive(_ context.Context, _ string) (string, error) {
	return "", fmt.Errorf("cold shard %s is already archived", s.id)
}

// Query rewrites FROM logs to FROM read_parquet('path') and executes.
// Partitioned archives use a glob path and hive_partitioning for file pruning.
func (s *ColdShard) Query(query string) ([]*pb.ResultRow, error) {
	var readExpr string
	if strings.Contains(s.parquetPath, "*") {
		readExpr = fmt.Sprintf("read_parquet('%s', hive_partitioning=true)", s.parquetPath)
	} else {
		readExpr = fmt.Sprintf("read_parquet('%s')", s.parquetPath)
	}
	rewritten := strings.Replace(query, "FROM logs", "FROM "+readExpr, 1)
	sqlRows, err := s.db.Query(rewritten)
	if err != nil {
		return nil, fmt.Errorf("cold query %s: %w", s.id, err)
	}
	defer sqlRows.Close()
	return ScanResultRows(sqlRows)
}

// QueryToSink streams query results to a RowSink.
func (s *ColdShard) QueryToSink(query string, sink RowSink) {
	defer sink.SendSentinel(s.id)
	rows, err := s.Query(query)
	if err != nil {
		return
	}
	for _, r := range rows {
		sink.SendRow(r, s.id)
	}
}

// Ingest always returns an error — cold shards are read-only.
func (s *ColdShard) Ingest(rows []*pb.LogRow) error {
	return fmt.Errorf("cold shard %s is read-only", s.id)
}

// Close releases the in-memory DuckDB connection.
func (s *ColdShard) Close() error {
	return s.db.Close()
}
