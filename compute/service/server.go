package service

import (
	"context"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/google/uuid"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
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
	job := s.pool.NewJob(req.Stages, int(req.ShardCount), req.MergeKey, int(req.Limit), req.KeepLast)

	s.mu.Lock()
	s.jobs[jobID] = job
	s.mu.Unlock()

	log.Printf("job %s created: shards=%d merge_key=%q limit=%d stages=%v", jobID, req.ShardCount, req.MergeKey, req.Limit, req.Stages)
	return &pb.CreateJobResponse{JobId: jobID}, nil
}

func (s *Server) SendRows(stream pb.WeftCompute_SendRowsServer) error {
	// Per-shard row tallies so the per-request log shows the shape of
	// the incoming stream (useful signal that compute is actually
	// receiving traffic, not just that the RPC handler got called).
	rowsByShard := map[string]int64{}
	sentinelsByShard := map[string]bool{}
	var jobID string
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			var total int64
			for _, n := range rowsByShard {
				total += n
			}
			log.Printf("job %s SendRows closed: rows=%d shards=%d sentinels=%d by_shard=%v", jobID, total, len(rowsByShard), len(sentinelsByShard), rowsByShard)
			return stream.SendAndClose(&pb.SendRowsResponse{RowsReceived: total})
		}
		if err != nil {
			log.Printf("job %s SendRows error after %d rows: %v", jobID, sumMap(rowsByShard), err)
			return err
		}

		if jobID == "" {
			jobID = msg.JobId
		}

		s.mu.Lock()
		job, ok := s.jobs[msg.JobId]
		s.mu.Unlock()
		if !ok {
			return fmt.Errorf("unknown job: %s", msg.JobId)
		}

		if msg.Sentinel {
			sentinelsByShard[msg.ShardId] = true
			job.SendSentinel(msg.ShardId)
		} else if msg.Row != nil {
			rowsByShard[msg.ShardId]++
			job.SendRow(msg.Row, msg.ShardId)
		}
	}
}

func sumMap(m map[string]int64) int64 {
	var n int64
	for _, v := range m {
		n += v
	}
	return n
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
		log.Printf("job %s GetResult: err=%v", req.JobId, err)
		return &pb.GetResultResponse{Error: err.Error()}, nil
	}
	log.Printf("job %s GetResult: ok, output=%d bytes", req.JobId, len(text))
	return &pb.GetResultResponse{Text: text}, nil
}

func (s *Server) ExecuteQuery(ctx context.Context, req *pb.ExecuteQueryRequest) (*pb.ExecuteQueryResponse, error) {
	stream, err := s.pool.ExecuteQuery(req)
	if err != nil {
		return &pb.ExecuteQueryResponse{Error: err.Error()}, nil
	}
	lines, err := compute.DrainLineStream(stream)
	if err != nil {
		return &pb.ExecuteQueryResponse{Error: err.Error()}, nil
	}
	return &pb.ExecuteQueryResponse{Text: compute.LinesToText(lines)}, nil
}

func (s *Server) ExecuteQueryStream(req *pb.ExecuteQueryRequest, out pb.WeftCompute_ExecuteQueryStreamServer) error {
	stream, err := s.pool.ExecuteQuery(req)
	if err != nil {
		return out.Send(&pb.ExecuteQueryStreamResponse{Error: err.Error()})
	}
	const batchSize = 500
	var batch []string
	for {
		line, ok, err := stream.Next(out.Context())
		if err != nil {
			return out.Send(&pb.ExecuteQueryStreamResponse{Error: err.Error()})
		}
		if !ok {
			break
		}
		batch = append(batch, line.Raw)
		if len(batch) >= batchSize {
			if err := out.Send(&pb.ExecuteQueryStreamResponse{Lines: batch}); err != nil {
				return err
			}
			batch = nil
		}
	}
	if len(batch) > 0 {
		if err := out.Send(&pb.ExecuteQueryStreamResponse{Lines: batch}); err != nil {
			return err
		}
	}
	return nil
}

// Verify at compile time that ComputeJob implements RowSink.
var _ shard.RowSink = (compute.ComputeJob)(nil)
