package compute

import (
	"fmt"
	"log"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// ComputeMessage is sent from shards to a compute job. Either Row is set
// (a data row) or Sentinel is true (indicating the shard has finished).
type ComputeMessage struct {
	Row      *pb.ResultRow
	ShardID  string
	Sentinel bool
}

// ComputePool creates compute jobs. Implementations:
//   - LocalComputePool: in-process execution via warp's bash interpreter
//   - GRPCComputePool: fans out to remote compute workers
type ComputePool interface {
	NewJob(stages []string, shardCount int, mergeKey string, limit int) ComputeJob

	// ExecuteQuery fans out SQL to the given shards, merges results,
	// applies pipeline stages, and returns the output. The caller
	// provides shard descriptors so compute does all shard I/O.
	ExecuteQuery(req *pb.ExecuteQueryRequest) (string, error)
}

// DialShard creates a shard.Shard from a proto ShardDescriptor.
// For cold shards (Lambda-backed), creds and region enable SigV4
// request signing. Pass nil creds to skip signing.
func DialShard(d *pb.ShardDescriptor, secret string, creds aws.CredentialsProvider, region string) (shard.Shard, error) {
	if d.GrpcAddr != "" {
		return shard.NewGRPCShard(d.ShardId, d.GrpcAddr, secret)
	}
	if d.LambdaUrl != "" {
		cs, err := shard.NewColdShard(d.ShardId, d.Apps, d.ParquetPath, d.LambdaUrl, nil)
		if err != nil {
			return nil, err
		}
		if creds != nil {
			cs.WithAWSAuth(creds, region)
		}
		return cs, nil
	}
	return nil, fmt.Errorf("shard %s: no grpc_addr or lambda_url", d.ShardId)
}

// FanOutToShards dials each shard, runs QueryToSink, and waits.
func FanOutToShards(req *pb.ExecuteQueryRequest, job ComputeJob, creds aws.CredentialsProvider, region string) {
	var wg sync.WaitGroup
	wg.Add(len(req.Shards))
	for _, desc := range req.Shards {
		go func(d *pb.ShardDescriptor) {
			defer wg.Done()
			sh, err := DialShard(d, req.ShardSecret, creds, region)
			if err != nil {
				log.Printf("execute-query: dial shard %s: %v", d.ShardId, err)
				job.SendSentinel(d.ShardId)
				return
			}
			defer sh.Close()
			sh.QueryToSink(req.Sql, job)
		}(desc)
	}
	wg.Wait()
}

// ComputeJob receives rows from shards and produces output as a stream
// of lines. It implements shard.RowSink so shards can stream directly
// to it.
type ComputeJob interface {
	shard.RowSink
	// Send delivers a row or sentinel from a shard. Safe for concurrent use.
	Send(msg ComputeMessage)
	// Stream returns a forward-only line stream of the job's output.
	// Multiple calls return independent views over the same underlying
	// result; the underlying pipeline runs once.
	Stream() LineStream
	// Result blocks until all sentinels are received and the pipeline has
	// finished executing, then returns the full text output. Convenience
	// wrapper over Stream for callers that want the whole blob.
	Result() (string, error)
}
