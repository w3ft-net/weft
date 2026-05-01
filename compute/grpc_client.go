package compute

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// GRPCComputePool implements ComputePool by connecting to a remote compute
// worker over gRPC. Drop-in replacement for LocalComputePool.
type GRPCComputePool struct {
	Addr   string
	conn   *grpc.ClientConn
	client pb.WeftComputeClient
}

// NewGRPCComputePool connects to a remote compute worker.
func NewGRPCComputePool(addr string) (*GRPCComputePool, error) {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("connect to compute at %s: %w", addr, err)
	}
	return &GRPCComputePool{
		Addr:   addr,
		conn:   conn,
		client: pb.NewWeftComputeClient(conn),
	}, nil
}

// Close closes the gRPC connection.
func (p *GRPCComputePool) Close() error {
	return p.conn.Close()
}

func (p *GRPCComputePool) NewJob(stages []string, shardCount int, mergeKey string, limit int) ComputeJob {
	resp, err := p.client.CreateJob(context.Background(), &pb.CreateJobRequest{
		Stages:     stages,
		ShardCount: int32(shardCount),
		MergeKey:   mergeKey,
		Limit:      int32(limit),
	})
	if err != nil {
		// Return a job that immediately errors on Result().
		return &errJob{err: fmt.Errorf("create remote job: %w", err)}
	}
	return &GRPCComputeJob{
		pool:       p,
		jobID:      resp.JobId,
		shardCount: int32(shardCount),
	}
}

// GRPCComputeJob implements ComputeJob by streaming rows to a remote compute
// worker and waiting for the result.
type GRPCComputeJob struct {
	pool       *GRPCComputePool
	jobID      string
	shardCount int32
	sentinels  atomic.Int32
	streamMu   sync.Mutex
	stream     pb.WeftCompute_SendRowsClient
}

func (j *GRPCComputeJob) ensureStream() error {
	if j.stream != nil {
		return nil
	}
	var err error
	j.stream, err = j.pool.client.SendRows(context.Background())
	return err
}

// Send implements ComputeJob.
func (j *GRPCComputeJob) Send(msg ComputeMessage) {
	j.streamMu.Lock()
	defer j.streamMu.Unlock()

	if err := j.ensureStream(); err != nil {
		return
	}

	pbMsg := &pb.ComputeMessage{
		JobId:    j.jobID,
		ShardId:  msg.ShardID,
		Sentinel: msg.Sentinel,
	}
	if msg.Row != nil {
		pbMsg.Row = msg.Row
	}
	j.stream.Send(pbMsg)
}

// SendRow implements shard.RowSink.
func (j *GRPCComputeJob) SendRow(row *pb.ResultRow, shardID string) {
	j.Send(ComputeMessage{Row: row, ShardID: shardID})
}

// SendSentinel implements shard.RowSink.
func (j *GRPCComputeJob) SendSentinel(shardID string) {
	j.Send(ComputeMessage{ShardID: shardID, Sentinel: true})

	// Only close the stream when all shards have sent their sentinel.
	if j.sentinels.Add(1) >= j.shardCount {
		j.streamMu.Lock()
		if j.stream != nil {
			j.stream.CloseAndRecv()
		}
		j.streamMu.Unlock()
	}
}

// Result blocks until the remote job completes and returns the output.
func (j *GRPCComputeJob) Result() (string, error) {
	resp, err := j.pool.client.GetResult(context.Background(), &pb.GetResultRequest{
		JobId: j.jobID,
	})
	if err != nil {
		return "", fmt.Errorf("get result: %w", err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("compute error: %s", resp.Error)
	}
	return resp.Text, nil
}

// Stream wraps the remote Result call in a slice-backed line stream.
// The compute gRPC is still single-shot — the whole text comes back
// in one response, gets split into Lines, and replays. A future
// streaming compute RPC would replace this with a channel pumped
// directly from the wire.
func (j *GRPCComputeJob) Stream() LineStream {
	text, err := j.Result()
	if err != nil {
		return newSliceLineStream(nil, err)
	}
	if text == "" {
		return newSliceLineStream(nil, nil)
	}
	parts := strings.Split(strings.TrimRight(text, "\n"), "\n")
	lines := make([]Line, len(parts))
	for i, p := range parts {
		lines[i] = Line{Raw: p}
	}
	return newSliceLineStream(lines, nil)
}

// MergedRows synthesizes a Rows-shape result from the remote text
// output. LineNum is zeroed — the wire doesn't carry per-row metadata
// today. Used by the coordinator transitionally for zero-stage
// queries; phase 4 deletes the type-assertion in executeQuery once the
// wire moves to LineStream and Rows-shape return is gone.
func (j *GRPCComputeJob) MergedRows() ([]*pb.ResultRow, error) {
	text, err := j.Result()
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	rows := make([]*pb.ResultRow, len(lines))
	for i, line := range lines {
		rows[i] = &pb.ResultRow{Raw: line}
	}
	return rows, nil
}

// errJob is a ComputeJob that immediately returns an error.
type errJob struct {
	err error
}

func (j *errJob) Send(ComputeMessage)            {}
func (j *errJob) SendRow(*pb.ResultRow, string)  {}
func (j *errJob) SendSentinel(string)            {}
func (j *errJob) Result() (string, error)        { return "", j.err }
func (j *errJob) Stream() LineStream {
	return newSliceLineStream(nil, j.err)
}

