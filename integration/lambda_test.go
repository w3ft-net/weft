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

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

// TestLambdaColdShard_DockerIntegration tests the full flow:
// ingest → export to MinIO → LambdaColdShard queries via the lambda-shard container.
//
// Requires docker-compose stack running (lambda-shard + minio).
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
		{Raw: "lambda integration row 1", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: t0.Add(1 * time.Second).UnixNano()},
		{Raw: "lambda integration row 2", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: t0.Add(2 * time.Second).UnixNano()},
		{Raw: "lambda integration row 3", App: "callbox", Level: "WARN", Customer: "kevlar", TsUnixNs: t0.Add(3 * time.Second).UnixNano()},
		{Raw: "lambda integration row 4", App: "vimgym", Level: "DEBUG", Customer: "acme", TsUnixNs: t0.Add(4 * time.Second).UnixNano()},
	}
	if err := ds.Ingest(rows); err != nil {
		t.Fatal(err)
	}

	// Export to MinIO S3.
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
	// Note: the container reads from MinIO using its own S3_* env vars
	// (set in docker-compose), so the Parquet path uses the container's
	// MinIO endpoint (minio:9000), not localhost:9050.
	containerS3Path := fmt.Sprintf("s3://%s/lambda-test/shard-lambda-int.parquet", ColdBucket)

	cs, err := shard.NewColdShard("cold-lambda", []string{"vimgym", "callbox"}, containerS3Path, lambdaURL, nil)
	if err != nil {
		t.Fatalf("create cold shard: %v", err)
	}

	// Query all kevlar rows.
	result, err := cs.Query("SELECT raw, ts FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatalf("query via Lambda: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 kevlar rows, got %d", len(result))
	}
	t.Logf("queried %d rows via Lambda container", len(result))

	// Filter by app.
	result, err = cs.Query("SELECT raw, ts FROM logs WHERE app = 'vimgym' AND customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 vimgym/kevlar rows, got %d", len(result))
	}

	// Filter by level.
	result, err = cs.Query("SELECT raw, ts FROM logs WHERE level = 'ERROR' AND customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 ERROR row, got %d", len(result))
	}

	// QueryToSink.
	sink := &testSink{}
	cs.QueryToSink("SELECT raw, ts FROM logs WHERE customer = 'kevlar'", nil, sink)
	if !sink.sentinelReceived {
		t.Error("expected sentinel")
	}
	if len(sink.rows) != 3 {
		t.Fatalf("expected 3 rows via sink, got %d", len(sink.rows))
	}
}

// Verify the exported Parquet is on MinIO.
func TestLambdaColdShard_VerifyS3Upload(t *testing.T) {
	cfg, _ := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(MinioAccessKey, MinioSecretKey, "")),
	)
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("http://%s", MinioEndpoint))
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
