package service

import (
	"errors"
	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// Server implements the WeftCompute gRPC service.
type Server struct {
	pb.UnimplementedWeftComputeServer
	engine *compute.Engine
}

// NewServer creates a new compute gRPC server backed by the given engine.
func NewServer(engine *compute.Engine) *Server {
	return &Server{engine: engine}
}

func (s *Server) ExecuteQueryStream(req *pb.ExecuteQueryRequest, out pb.WeftCompute_ExecuteQueryStreamServer) error {
	ctx := out.Context()
	ctx, span := weftsdk.StartSpan(ctx, "compute.ExecuteQueryStream")
	defer span.End()
	stream, err := s.engine.ExecuteQuery(ctx, req)
	if err != nil {
		span.RecordError(err)
		return out.Send(&pb.ExecuteQueryStreamResponse{Error: err.Error()})
	}
	const batchSize = 500
	var batch []string
	for {
		line, ok, err := stream.Next(ctx)
		if err != nil {
			span.RecordError(err)
			resp := &pb.ExecuteQueryStreamResponse{Error: err.Error()}
			var missing *compute.MissingRowsError
			if errors.As(err, &missing) {
				resp.MissingShard, resp.Error = missing.ShardID, missing.Err.Error()
			}
			return out.Send(resp)
		}
		if !ok {
			break
		}
		batch = append(batch, line.Raw)
		if len(batch) >= batchSize {
			if err := out.Send(&pb.ExecuteQueryStreamResponse{Lines: batch}); err != nil {
				span.RecordError(err)
				return err
			}
			batch = nil
		}
	}
	// The last message carries the cold scan count, which is complete only
	// now. Lines and the count share it, so an older coordinator, which
	// ignores the field, reads the lines as before.
	final := &pb.ExecuteQueryStreamResponse{Lines: batch}
	if sc, ok := stream.(compute.ScanCounter); ok {
		final.RowsScanned = sc.RowsScanned()
	}
	if len(final.Lines) > 0 || final.RowsScanned != 0 {
		if err := out.Send(final); err != nil {
			span.RecordError(err)
			return err
		}
	}
	return nil
}
