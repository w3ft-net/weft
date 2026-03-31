package service

import (
	"context"
	"fmt"
	"time"

	"github.com/kevlarcon/weft/configstore"
	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	shardservice "github.com/kevlarcon/weft/shard/service"
)

// Server implements the WeftCoordinator gRPC service.
type Server struct {
	pb.UnimplementedWeftCoordinatorServer
	Coord *coordinator.Coordinator
	Store *configstore.Store // nil if no config store (dev mode)
	Apps  []string
}

func (s *Server) Query(_ context.Context, req *pb.CoordinatorQueryRequest) (*pb.CoordinatorQueryResponse, error) {
	tr := coordinator.TimeRange{
		Start: time.Unix(0, req.TimeStartNs),
		End:   time.Unix(0, req.TimeEndNs),
	}
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
	// If we have a config store, derive apps from enabled sources.
	if s.Store != nil {
		sources, err := s.Store.EnabledSources()
		if err == nil && len(sources) > 0 {
			var apps []string
			for _, src := range sources {
				apps = append(apps, src.App)
			}
			return &pb.ListAppsResponse{Apps: apps}, nil
		}
	}
	return &pb.ListAppsResponse{Apps: s.Apps}, nil
}

func (s *Server) AddSource(_ context.Context, req *pb.AddSourceRequest) (*pb.AddSourceResponse, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store not available")
	}
	err := s.Store.AddSource(configstore.Source{
		App:          req.App,
		Customer:     req.Customer,
		LogGroup:     req.LogGroup,
		Region:       req.Region,
		SchemaYAML:   req.SchemaYaml,
		PollInterval: req.PollInterval,
		Enabled:      true,
	})
	if err != nil {
		return nil, err
	}
	return &pb.AddSourceResponse{}, nil
}

func (s *Server) ListSources(_ context.Context, _ *pb.ListSourcesRequest) (*pb.ListSourcesResponse, error) {
	if s.Store == nil {
		return &pb.ListSourcesResponse{}, nil
	}
	sources, err := s.Store.ListSources()
	if err != nil {
		return nil, err
	}
	var infos []*pb.SourceInfo
	for _, src := range sources {
		infos = append(infos, &pb.SourceInfo{
			App:        src.App,
			Customer:   src.Customer,
			LogGroup:   src.LogGroup,
			Region:     src.Region,
			Enabled:    src.Enabled,
			LastPollAt: src.LastPollAt,
		})
	}
	return &pb.ListSourcesResponse{Sources: infos}, nil
}

func (s *Server) RemoveSource(_ context.Context, req *pb.RemoveSourceRequest) (*pb.RemoveSourceResponse, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store not available")
	}
	if err := s.Store.RemoveSource(req.App); err != nil {
		return nil, err
	}
	return &pb.RemoveSourceResponse{}, nil
}
