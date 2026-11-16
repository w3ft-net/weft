package compute

import (
	"context"
	"fmt"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/w3ft-net/weft/backends/logs/archive"
	"github.com/w3ft-net/weft/backends/logs/archive/parquet"
	"github.com/w3ft-net/weft/backends/logs/live"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// ArchiveReaderLambda is the name configuration gives the Lambda reader: the
// archive reader of the open build, which runs DuckDB over the Parquet in a
// Lambda function and is reached at the URL in each archived shard's descriptor.
const ArchiveReaderLambda = "duckdb-lambda"

// NewArchiveRouter builds what a compute process reads archives through: the
// formats and readers this build links, and the configured choice of reader for
// each format. readers maps a format ID, or archive.Default, to a reader's
// name. There must be at least one: with none, an archived shard could never be
// read, and a process that starts anyway only finds out on the first query.
//
// A build with another reader registers it here, beside the Lambda one.
func NewArchiveRouter(readers map[string]string, creds aws.CredentialsProvider, region string) (*archive.Router, error) {
	reg := archive.NewRegistry()
	for _, f := range parquet.Formats() {
		if err := reg.AddFormat(f); err != nil {
			return nil, err
		}
	}
	if err := reg.AddReader(ArchiveReaderLambda, shard.LambdaReader(creds, region)); err != nil {
		return nil, err
	}
	return reg.Readers(readers)
}

// archiveShard is a reader's handle as a shard, for a reader whose handle is not
// already one. An archive is read-only: it is not ingested into or archived.
type archiveShard struct {
	id string
	h  archive.Handle
}

func (s *archiveShard) ID() string { return s.id }

func (s *archiveShard) QueryToSink(ctx context.Context, sql string, args []any, sink shard.RowSink) {
	s.h.QueryToSink(ctx, sql, args, sink)
}

func (s *archiveShard) Query(ctx context.Context, sql string, args ...any) ([]*pb.ResultRow, error) {
	c := &collect{}
	s.h.QueryToSink(ctx, sql, args, c)
	if c.err != nil {
		return nil, c.err
	}
	return c.rows, nil
}

func (s *archiveShard) Ingest(context.Context, []*pb.LogRow, shard.IngestRecorder) error {
	return fmt.Errorf("archived shard %s is read-only", s.id)
}

func (s *archiveShard) IngestBatch(ctx context.Context, b shard.Batch, rec shard.IngestRecorder) error {
	return s.Ingest(ctx, b.Rows, rec)
}

func (s *archiveShard) Archive(context.Context, string) (string, []shard.PartitionStat, error) {
	return "", nil, fmt.Errorf("archived shard %s is already archived", s.id)
}

func (s *archiveShard) Close() error { return s.h.Close() }

// collect gathers a stream into rows.
type collect struct {
	mu   sync.Mutex
	rows []*pb.ResultRow
	err  error
}

var _ live.RowSink = (*collect)(nil)

func (c *collect) SendRow(row *pb.ResultRow, _ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, row)
}

func (c *collect) Fail(_ string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func (c *collect) SendSentinel(string) {}
