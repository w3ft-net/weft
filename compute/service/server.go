package service

import (
	"context"

	"google.golang.org/grpc/metadata"

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

// ctxWithTraceIDFromMetadata reads the `weft-trace-id` gRPC metadata a
// delegated caller (the coordinator) ferried across this boundary —
// context.Value doesn't cross a gRPC call on its own, so this
// re-establishes it as an ambient ctx value for everything downstream
// (FanOutToShards, shard.QueryToSink) that reads it via
// weftsdk.TraceIDFromContext. Mirrors coordinator/service/server.go's
// identical read for the api→coordinator boundary.
func ctxWithTraceIDFromMetadata(ctx context.Context) context.Context {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if ids := md.Get("weft-trace-id"); len(ids) > 0 {
			ctx = weftsdk.ContextWithTraceID(ctx, ids[0])
		}
		// Same courier, for the span coordinator's own "coordinator.Execute"
		// started (if any) — read before StartSpan below so this
		// process's span parents off it instead of starting a fresh root.
		if ids := md.Get("weft-span-id"); len(ids) > 0 {
			ctx = weftsdk.ContextWithSpanID(ctx, ids[0])
		}
	}
	return ctx
}

func (s *Server) ExecuteQueryStream(req *pb.ExecuteQueryRequest, out pb.WeftCompute_ExecuteQueryStreamServer) error {
	ctx := ctxWithTraceIDFromMetadata(out.Context())
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
			return out.Send(&pb.ExecuteQueryStreamResponse{Error: err.Error()})
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
	if len(batch) > 0 {
		if err := out.Send(&pb.ExecuteQueryStreamResponse{Lines: batch}); err != nil {
			span.RecordError(err)
			return err
		}
	}
	return nil
}
