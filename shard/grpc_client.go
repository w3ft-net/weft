package shard

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// GRPCShard implements the Shard interface by connecting to a remote shard
// over gRPC. Drop-in replacement for DuckDBShard in the coordinator.
type GRPCShard struct {
	id          string
	addr        string
	windowStart time.Time
	windowEnd   time.Time
	conn        *grpc.ClientConn
	client      pb.ShardQueryClient
}

// NewGRPCShard connects to a remote shard gRPC server.
func NewGRPCShard(id, addr string, start, end time.Time) (*GRPCShard, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to shard %s at %s: %w", id, addr, err)
	}
	return &GRPCShard{
		id:          id,
		addr:        addr,
		windowStart: start,
		windowEnd:   end,
		conn:        conn,
		client:      pb.NewShardQueryClient(conn),
	}, nil
}

func (s *GRPCShard) ID() string            { return s.id }
func (s *GRPCShard) WindowStart() time.Time { return s.windowStart }
func (s *GRPCShard) WindowEnd() time.Time   { return s.windowEnd }

func (s *GRPCShard) Close() error {
	return s.conn.Close()
}

// Ingest sends a batch of rows to the remote shard for insertion.
func (s *GRPCShard) Ingest(rows []LogRow) error {
	pbRows := make([]*pb.LogRow, len(rows))
	for i := range rows {
		pbRows[i] = &pb.LogRow{
			LineNum:     rows[i].LineNum,
			Raw:         rows[i].Raw,
			App:         rows[i].App,
			Level:       rows[i].Level,
			TsUnixNs:   rows[i].Ts.UnixNano(),
			Instance:    rows[i].Instance,
			PayloadJson: rows[i].Payload,
			Fields:      rows[i].Fields,
			Customer:    rows[i].Customer,
		}
	}
	_, err := s.client.Ingest(context.Background(), &pb.IngestRequest{Rows: pbRows})
	if err != nil {
		return fmt.Errorf("ingest to shard %s: %w", s.id, err)
	}
	return nil
}

// Query executes SQL on the remote shard and collects all rows.
func (s *GRPCShard) Query(sql string) ([]LogRow, error) {
	stream, err := s.client.Query(context.Background(), &pb.QueryRequest{Sql: sql})
	if err != nil {
		return nil, fmt.Errorf("query shard %s: %w", s.id, err)
	}

	var rows []LogRow
	for {
		pbRow, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("recv from shard %s: %w", s.id, err)
		}
		rows = append(rows, protoToLogRow(pbRow))
	}
	return rows, nil
}

// QueryToSink executes SQL on the remote shard and streams rows to the sink.
func (s *GRPCShard) QueryToSink(sql string, sink RowSink) {
	defer sink.SendSentinel(s.id)

	stream, err := s.client.Query(context.Background(), &pb.QueryRequest{Sql: sql})
	if err != nil {
		return
	}

	for {
		pbRow, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return
		}
		row := protoToLogRow(pbRow)
		sink.SendRow(&row, s.id)
	}
}

func protoToLogRow(r *pb.LogRow) LogRow {
	return LogRow{
		LineNum:  r.LineNum,
		Raw:      r.Raw,
		App:      r.App,
		Level:    r.Level,
		Ts:       time.Unix(0, r.TsUnixNs),
		Instance: r.Instance,
		Payload:  r.PayloadJson,
		Fields:   r.Fields,
		Customer: r.Customer,
	}
}
