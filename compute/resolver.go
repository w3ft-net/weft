package compute

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// Resolver is the compute-side abstraction over "how do I materialize
// a shard.Shard handle for this shard id." Production has two impls
// (LiveResolver, ColdResolver), tests add a third (InProcessResolver
// in package computetest). Compute's job is to call Dial and run
// QueryToSink — it doesn't know or care whether the resulting handle
// is a gRPC client, a Lambda-backed cold reader, or a direct pointer
// to an in-process DuckDB shard.
//
// Resolvers are constructed by the caller (coordinator side for live
// queries, the remote compute worker for ExecuteQueryRequest off the
// wire). The pool caches the Dial result keyed by ShardID so repeat
// queries against the same shard don't re-open the gRPC connection.
type Resolver interface {
	// ShardID returns the registry id for this shard. Used as the
	// cache key by LocalComputePool.
	ShardID() string

	// Dial materializes the shard.Shard handle. Called once per
	// (shard, addr) combination; the pool caches the result. May
	// open gRPC connections, build SigV4 signers, or simply return
	// a pre-built handle depending on the impl.
	Dial() (shard.Shard, error)
}

// LiveResolver dials a hot shard over gRPC. The bearer secret is the
// SHARD_SECRET that the shard-side interceptor checks against; empty
// is only valid in tests where the shard runs without interceptors.
type LiveResolver struct {
	ID     string
	Addr   string
	Secret string
}

func (r *LiveResolver) ShardID() string { return r.ID }

func (r *LiveResolver) Dial() (shard.Shard, error) {
	return shard.NewGRPCShard(r.ID, r.Addr, r.Secret)
}

// ColdResolver builds a Lambda-backed cold shard handle for an
// archived shard. Creds + region enable SigV4 request signing on the
// Lambda Function URL; pass nil creds to skip signing (used by
// localstack-style integration tests).
type ColdResolver struct {
	ID          string
	LambdaURL   string
	ParquetPath string
	Apps        []string
	Creds       aws.CredentialsProvider
	Region      string
}

func (r *ColdResolver) ShardID() string { return r.ID }

func (r *ColdResolver) Dial() (shard.Shard, error) {
	cs, err := shard.NewColdShard(r.ID, r.Apps, r.ParquetPath, r.LambdaURL, nil)
	if err != nil {
		return nil, err
	}
	if r.Creds != nil {
		cs.WithAWSAuth(r.Creds, r.Region)
	}
	return cs, nil
}

// ResolverFromDescriptor is the production dispatcher: given a proto
// descriptor, build the right Resolver. Returns an error when neither
// GrpcAddr nor LambdaUrl is set — that's the in-process case, which
// must have been registered separately via pool.RegisterResolver
// before the query ran. Compute never invents handles; if a
// descriptor doesn't carry a dialing recipe and nothing's been
// registered for its id, the caller gets a clear error rather than
// a silent zero-row result.
func ResolverFromDescriptor(d *pb.ShardDescriptor, secret string, creds aws.CredentialsProvider, region string) (Resolver, error) {
	if d.GrpcAddr != "" {
		return &LiveResolver{ID: d.ShardId, Addr: d.GrpcAddr, Secret: secret}, nil
	}
	if d.LambdaUrl != "" {
		return &ColdResolver{
			ID:          d.ShardId,
			LambdaURL:   d.LambdaUrl,
			ParquetPath: d.ParquetPath,
			Apps:        d.Apps,
			Creds:       creds,
			Region:      region,
		}, nil
	}
	return nil, fmt.Errorf("shard %s: descriptor carries no grpc_addr or lambda_url and no resolver override registered", d.ShardId)
}
