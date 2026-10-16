//go:build integration && !localstack

package integration

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/graybeardcorp/weft/compute"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// TestLambdaColdShard_DockerIntegration tests the full flow:
// ingest → export to S3 (LocalStack) → LambdaColdShard queries via the lambda-shard container.
//
// Requires docker-compose stack running (lambda-shard + localstack).
func TestLambdaColdShard_DockerIntegration(t *testing.T) {
	// Lambda shard container runs on port 9030.
	lambdaURL := "http://localhost:9030"

	// Ingest test data.
	ds, err := shard.NewDuckDBShard("lambda-int", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{Raw: "lambda integration row 1", Namespace: "vimgym", Level: "ERROR", CustomerId: 1, TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
		{Raw: "lambda integration row 2", Namespace: "vimgym", Level: "INFO", CustomerId: 1, TsUnixNs: t0.Add(2 * time.Second).UnixNano()},
		{Raw: "lambda integration row 3", Namespace: "callbox", Level: "WARN", CustomerId: 1, TsUnixNs: t0.Add(3 * time.Second).UnixNano()},
		{Raw: "lambda integration row 4", Namespace: "vimgym", Level: "DEBUG", CustomerId: 5, TsUnixNs: t0.Add(4 * time.Second).UnixNano()},
	}
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}

	// Export to S3 (LocalStack).
	s3cfg := shard.S3ConfigFromEnv()
	if s3cfg == nil {
		t.Fatal("S3 config not set")
	}

	s3Path := fmt.Sprintf("s3://%s/lambda-test/shard-lambda-int.parquet", ColdBucket)
	if err := ds.ExportToS3Parquet(s3Path, s3cfg); err != nil {
		t.Fatalf("export to S3: %v", err)
	}
	t.Logf("exported to %s", s3Path)

	// Create LambdaColdShard pointing at the container.
	// Note: the container reads from LocalStack using its own S3_* env
	// vars (set in docker-compose), so the Parquet path uses the
	// container's LocalStack endpoint (localstack:4566), not localhost:4566.
	containerS3Path := fmt.Sprintf("s3://%s/lambda-test/shard-lambda-int.parquet", ColdBucket)

	cs, err := shard.NewColdShard("cold-lambda", []string{"vimgym", "callbox"}, containerS3Path, lambdaURL, nil)
	if err != nil {
		t.Fatalf("create cold shard: %v", err)
	}

	// Query all kevlar rows.
	result, err := cs.Query("SELECT raw, ts FROM logs WHERE customer_id = 1")
	if err != nil {
		t.Fatalf("query via Lambda: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 kevlar rows, got %d", len(result))
	}
	t.Logf("queried %d rows via Lambda container", len(result))

	// Filter by app.
	result, err = cs.Query("SELECT raw, ts FROM logs WHERE namespace = 'vimgym' AND customer_id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 vimgym/kevlar rows, got %d", len(result))
	}

	// Filter by level.
	result, err = cs.Query("SELECT raw, ts FROM logs WHERE level = 'ERROR' AND customer_id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 ERROR row, got %d", len(result))
	}

	// QueryToSink.
	sink := &testSink{}
	cs.QueryToSink(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1", nil, sink)
	if !sink.sentinelReceived {
		t.Error("expected sentinel")
	}
	if len(sink.rows) != 3 {
		t.Fatalf("expected 3 rows via sink, got %d", len(sink.rows))
	}
}

// TestComputeLifecycle_LambdaColdShardReachable is the system-level
// guard for the prod incident: a compute instance must be able to
// reach a Lambda-backed cold shard and return rows.
//
// Unlike TestLambdaColdShard_DockerIntegration (which calls ColdShard
// directly), this drives the real compute lifecycle —
// Engine.ExecuteQuery → FanOutToShards → DialShard →
// ColdShard — configured exactly as cmd/compute/main.go configures it,
// including compute.ResolveRegion(). AWS_REGION is unset here (the
// prod compute-instance state before the deploy fix), so this also
// asserts the region resolver is wired into the lifecycle: an empty
// region would 403 against real Lambda IAM auth.
func TestComputeLifecycle_LambdaColdShardReachable(t *testing.T) {
	t.Setenv("AWS_REGION", "")

	lambdaURL := "http://localhost:9030"

	ds, err := shard.NewDuckDBShard("compute-lifecycle", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{Raw: "compute lifecycle row 1", Namespace: "vimgym", Level: "ERROR", CustomerId: 1, TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
		{Raw: "compute lifecycle row 2", Namespace: "vimgym", Level: "INFO", CustomerId: 1, TsUnixNs: t0.Add(2 * time.Second).UnixNano()},
		{Raw: "compute lifecycle row 3", Namespace: "callbox", Level: "WARN", CustomerId: 1, TsUnixNs: t0.Add(3 * time.Second).UnixNano()},
		{Raw: "compute lifecycle row 4", Namespace: "vimgym", Level: "DEBUG", CustomerId: 5, TsUnixNs: t0.Add(4 * time.Second).UnixNano()},
	}
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}

	s3cfg := shard.S3ConfigFromEnv()
	if s3cfg == nil {
		t.Fatal("S3 config not set")
	}
	s3Path := fmt.Sprintf("s3://%s/lambda-test/shard-compute-lifecycle.parquet", ColdBucket)
	if err := ds.ExportToS3Parquet(s3Path, s3cfg); err != nil {
		t.Fatalf("export to S3: %v", err)
	}

	// Configure the pool exactly as cmd/compute/main.go does.
	pool := &compute.Engine{AWSRegion: compute.ResolveRegion()}

	stream, err := pool.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
		Sql: "SELECT raw, ts FROM logs WHERE customer_id = 1",
		Shards: []*pb.ShardDescriptor{{
			ShardId:     "cold-compute-lifecycle",
			LambdaUrl:   lambdaURL,
			ParquetPath: s3Path,
			Services:    []string{"vimgym", "callbox"},
		}},
	})
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}

	ctx := context.Background()
	var got int
	for {
		line, ok, nerr := stream.Next(ctx)
		if !ok {
			if nerr != nil {
				t.Fatalf("stream error: %v", nerr)
			}
			break
		}
		if line.Raw != "" {
			got++
		}
	}
	_ = stream.Close()

	if got != 3 {
		t.Fatalf("compute lifecycle reached lambda but got %d kevlar rows, want 3 "+
			"(0 rows is the silent-failure signature of the region 403)", got)
	}
}

// Verify the exported Parquet is on S3 (LocalStack).
func TestLambdaColdShard_VerifyS3Upload(t *testing.T) {
	cfg, _ := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(S3AccessKey, S3SecretKey, "")),
	)
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("http://%s", S3Endpoint))
		o.UsePathStyle = true
	})

	out, err := client.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: aws.String(ColdBucket),
		Prefix: aws.String("lambda-test/"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) == 0 {
		t.Fatal("expected at least one object in lambda-test/ prefix")
	}
	for _, obj := range out.Contents {
		t.Logf("S3 object: %s (%d bytes)", *obj.Key, *obj.Size)
	}
}

type testSink struct {
	rows             []*pb.ResultRow
	sentinelReceived bool
}

func (s *testSink) SendRow(row *pb.ResultRow, shardID string) {
	s.rows = append(s.rows, row)
}
func (s *testSink) SendSentinel(shardID string) {
	s.sentinelReceived = true
}
