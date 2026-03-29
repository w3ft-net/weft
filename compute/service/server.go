package service

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"

	"github.com/kevlarcon/weft/compute"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	shardservice "github.com/kevlarcon/weft/shard/service"
)

// Server implements the WeftCompute gRPC service.
type Server struct {
	pb.UnimplementedWeftComputeServer
	pool compute.ComputePool

	mu   sync.Mutex
	jobs map[string]compute.ComputeJob
}

// NewServer creates a new compute gRPC server backed by the given pool.
func NewServer(pool compute.ComputePool) *Server {
	return &Server{
		pool: pool,
		jobs: make(map[string]compute.ComputeJob),
	}
}

func (s *Server) CreateJob(_ context.Context, req *pb.CreateJobRequest) (*pb.CreateJobResponse, error) {
	jobID := uuid.New().String()
	job := s.pool.NewJob(req.Stages, int(req.ShardCount))

	s.mu.Lock()
	s.jobs[jobID] = job
	s.mu.Unlock()

	return &pb.CreateJobResponse{JobId: jobID}, nil
}

func (s *Server) SendRows(stream pb.WeftCompute_SendRowsServer) error {
	var count int64
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&pb.SendRowsResponse{RowsReceived: count})
		}
		if err != nil {
			return err
		}

		s.mu.Lock()
		job, ok := s.jobs[msg.JobId]
		s.mu.Unlock()
		if !ok {
			return fmt.Errorf("unknown job: %s", msg.JobId)
		}

		if msg.Sentinel {
			job.SendSentinel(msg.ShardId)
		} else if msg.Row != nil {
			row := shardservice.ProtoToLogRow(msg.Row)
			job.SendRow(&row, msg.ShardId)
			count++
		}
	}
}

func (s *Server) GetResult(_ context.Context, req *pb.GetResultRequest) (*pb.GetResultResponse, error) {
	s.mu.Lock()
	job, ok := s.jobs[req.JobId]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown job: %s", req.JobId)
	}

	text, err := job.Result()

	// Clean up the job.
	s.mu.Lock()
	delete(s.jobs, req.JobId)
	s.mu.Unlock()

	if err != nil {
		return &pb.GetResultResponse{Error: err.Error()}, nil
	}
	return &pb.GetResultResponse{Text: text}, nil
}

// Verify at compile time that ComputeJob implements RowSink.
var _ shard.RowSink = (compute.ComputeJob)(nil)
