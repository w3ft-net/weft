package compute

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
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

func (p *GRPCComputePool) NewJob(stages []string, shardCount int) ComputeJob {
	resp, err := p.client.CreateJob(context.Background(), &pb.CreateJobRequest{
		Stages:     stages,
		ShardCount: int32(shardCount),
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
		pbMsg.Row = logRowToProto(msg.Row)
	}
	j.stream.Send(pbMsg)
}

// SendRow implements shard.RowSink.
func (j *GRPCComputeJob) SendRow(row *shard.LogRow, shardID string) {
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

// errJob is a ComputeJob that immediately returns an error.
type errJob struct {
	err error
}

func (j *errJob) Send(ComputeMessage)               {}
func (j *errJob) SendRow(*shard.LogRow, string)      {}
func (j *errJob) SendSentinel(string)                {}
func (j *errJob) Result() (string, error)            { return "", j.err }

func logRowToProto(r *shard.LogRow) *pb.LogRow {
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
