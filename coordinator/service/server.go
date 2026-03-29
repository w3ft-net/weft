package service

import (
	"context"
	"time"

	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	shardservice "github.com/kevlarcon/weft/shard/service"
)

// Server implements the WeftCoordinator gRPC service.
type Server struct {
	pb.UnimplementedWeftCoordinatorServer
	Coord *coordinator.Coordinator
	Apps  []string
}

func (s *Server) Query(_ context.Context, req *pb.CoordinatorQueryRequest) (*pb.CoordinatorQueryResponse, error) {
	tr := coordinator.TimeRange{
		Start: time.Unix(0, req.TimeStartNs),
		End:   time.Unix(0, req.TimeEndNs),
	}
	// If no time range specified, use a wide default.
	if req.TimeStartNs == 0 && req.TimeEndNs == 0 {
		tr.Start = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		tr.End = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	}

	result, err := s.Coord.Query(req.Pipeline, tr)
	if err != nil {
		return nil, err
	}

	resp := &pb.CoordinatorQueryResponse{}
	if result.Text != "" {
		resp.Text = result.Text
	} else {
		for _, r := range result.Rows {
			resp.Rows = append(resp.Rows, shardservice.LogRowToProto(&r))
		}
	}
	return resp, nil
}

func (s *Server) ListApps(_ context.Context, _ *pb.ListAppsRequest) (*pb.ListAppsResponse, error) {
	return &pb.ListAppsResponse{Apps: s.Apps}, nil
}
