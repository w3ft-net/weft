package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/kevlarcon/weft/configstore"
	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	shardservice "github.com/kevlarcon/weft/shard/service"
)

// Server implements the WeftCoordinator gRPC service.
type Server struct {
	pb.UnimplementedWeftCoordinatorServer
	Coord *coordinator.Coordinator
	Store *configstore.Store
	Apps  []string
}

func (s *Server) Query(req *pb.CoordinatorQueryRequest, stream pb.WeftCoordinator_QueryServer) error {
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
		return err
	}

	if result.Text != "" {
		return stream.Send(&pb.CoordinatorQueryResponse{Text: result.Text})
	}

	// Stream rows in batches to stay under gRPC message size limit.
	const batchSize = 500
	for i := 0; i < len(result.Rows); i += batchSize {
		end := i + batchSize
		if end > len(result.Rows) {
			end = len(result.Rows)
		}
		resp := &pb.CoordinatorQueryResponse{}
		for _, r := range result.Rows[i:end] {
			resp.Rows = append(resp.Rows, shardservice.LogRowToProto(&r))
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) ListApps(_ context.Context, _ *pb.ListAppsRequest) (*pb.ListAppsResponse, error) {
	if s.Store != nil {
		apps, err := s.Store.AppNames()
		if err == nil && len(apps) > 0 {
			return &pb.ListAppsResponse{Apps: apps}, nil
		}
	}
	return &pb.ListAppsResponse{Apps: s.Apps}, nil
}

func (s *Server) AddSource(_ context.Context, req *pb.AddSourceRequest) (*pb.AddSourceResponse, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store not available")
	}

	customer, err := s.Store.FindOrCreateCustomer(req.Customer)
	if err != nil {
		return nil, fmt.Errorf("customer: %w", err)
	}

	account, err := s.Store.FindOrCreateAccount(customer.ID, req.AwsAccountId, req.RoleArn, req.ExternalId)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}

	pollInterval := req.PollInterval
	if pollInterval == "" {
		pollInterval = "30s"
	}
	if err := s.Store.AddLogGroup(account.ID, req.App, req.LogGroup, req.Region, req.SchemaYaml, pollInterval); err != nil {
		return nil, fmt.Errorf("log group: %w", err)
	}

	return &pb.AddSourceResponse{}, nil
}

func (s *Server) ListSources(_ context.Context, _ *pb.ListSourcesRequest) (*pb.ListSourcesResponse, error) {
	if s.Store == nil {
		return &pb.ListSourcesResponse{}, nil
	}
	logGroups, err := s.Store.AllLogGroups()
	if err != nil {
		return nil, err
	}
	var infos []*pb.SourceInfo
	for _, lg := range logGroups {
		infos = append(infos, &pb.SourceInfo{
			App:        lg.App,
			Customer:   lg.CustomerName,
			LogGroup:   lg.LogGroupName,
			Region:     lg.Region,
			Enabled:    lg.Enabled,
			LastPollAt: lg.LastPollAt,
		})
	}
	return &pb.ListSourcesResponse{Sources: infos}, nil
}

func (s *Server) RemoveSource(_ context.Context, req *pb.RemoveSourceRequest) (*pb.RemoveSourceResponse, error) {
	if s.Store == nil {
		return nil, fmt.Errorf("config store not available")
	}
	if err := s.Store.RemoveLogGroup(req.App); err != nil {
		return nil, err
	}
	return &pb.RemoveSourceResponse{}, nil
}

func (s *Server) IngestStream(stream pb.WeftCoordinator_IngestStreamServer) error {
	var rows []shard.LogRow
	var count int64

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		row := shard.LogRow{
			LineNum:  count + 1,
			Raw:      msg.Raw,
			App:      msg.App,
			Level:    msg.Level,
			Ts:       time.Unix(0, msg.TsUnixNs),
			Instance: msg.Instance,
			Customer: msg.Customer,
			Fields:   msg.Fields,
		}
		if len(msg.PayloadJson) > 0 {
			row.Payload = json.RawMessage(msg.PayloadJson)
		}
		rows = append(rows, row)
		count++

		// Batch ingest every 100 rows.
		if len(rows) >= 100 {
			if err := s.ingestToShard(rows); err != nil {
				log.Printf("ingest-stream: %v", err)
			}
			rows = rows[:0]
		}
	}

	// Flush remaining.
	if len(rows) > 0 {
		if err := s.ingestToShard(rows); err != nil {
			log.Printf("ingest-stream: %v", err)
		}
	}

	return stream.SendAndClose(&pb.IngestStreamResponse{RowsReceived: count})
}

func (s *Server) ingestToShard(rows []shard.LogRow) error {
	for _, sh := range s.Coord.Shards {
		if err := sh.Ingest(rows); err != nil {
			continue
		}
		return nil
	}
	return fmt.Errorf("all shards rejected ingest")
}
