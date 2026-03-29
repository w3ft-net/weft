package service

import (
	"context"
	"time"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// Server implements the ShardQuery gRPC service backed by a DuckDBShard.
type Server struct {
	pb.UnimplementedShardQueryServer
	shard *shard.DuckDBShard
}

// NewServer creates a new shard gRPC server.
func NewServer(s *shard.DuckDBShard) *Server {
	return &Server{shard: s}
}

// Query executes SQL and streams matching rows to the client.
func (s *Server) Query(req *pb.QueryRequest, stream pb.ShardQuery_QueryServer) error {
	rows, err := s.shard.Query(req.Sql)
	if err != nil {
		return err
	}

	for _, r := range rows {
		pbRow := LogRowToProto(&r)
		if err := stream.Send(pbRow); err != nil {
			return err
		}
	}
	return nil
}

// QueryToCompute is a placeholder — in Phase 1 the coordinator manages
// shard→compute streaming. In Phase 2 the shard would connect directly
// to the compute service.
func (s *Server) QueryToCompute(_ context.Context, req *pb.QueryToComputeRequest) (*pb.QueryToComputeResponse, error) {
	return &pb.QueryToComputeResponse{}, nil
}

// Ingest accepts a batch of rows for insertion.
func (s *Server) Ingest(_ context.Context, req *pb.IngestRequest) (*pb.IngestResponse, error) {
	rows := make([]shard.LogRow, len(req.Rows))
	for i, r := range req.Rows {
		rows[i] = ProtoToLogRow(r)
	}
	if err := s.shard.Ingest(rows); err != nil {
		return nil, err
	}
	return &pb.IngestResponse{RowsIngested: int64(len(rows))}, nil
}

// LogRowToProto converts a shard.LogRow to a protobuf LogRow.
func LogRowToProto(r *shard.LogRow) *pb.LogRow {
	return &pb.LogRow{
		LineNum:     r.LineNum,
		Raw:         r.Raw,
		App:         r.App,
		Level:       r.Level,
		TsUnixNs:   r.Ts.UnixNano(),
		Instance:    r.Instance,
		PayloadJson: r.Payload,
		Fields:      r.Fields,
		Customer:    r.Customer,
	}
}

// ProtoToLogRow converts a protobuf LogRow to a shard.LogRow.
func ProtoToLogRow(r *pb.LogRow) shard.LogRow {
	return shard.LogRow{
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
