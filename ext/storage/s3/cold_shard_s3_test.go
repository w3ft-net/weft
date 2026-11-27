package s3

import (
	"context"
	"fmt"
	"github.com/w3ft-net/weft/shard"
	_ "github.com/w3ft-net/weft/shard/duckbind"
	_ "github.com/w3ft-net/weft/shard/duckdbdrv"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	pb "github.com/w3ft-net/weft/proto/weftpb"
)

const (
	s3TestEndpoint  = "http://localhost:4566"
	s3TestAccessKey = "test"
	s3TestSecretKey = "test"
	s3TestBucket    = "weft-cold-test"
)

// TestColdShard_S3Integration tests the full flow:
// ingest → export to Parquet → upload to S3 → ColdShard reads from S3.
//
// Requires LocalStack running on localhost:4566 (docker compose up localstack).
// Skipped if LocalStack is not reachable.
func TestColdShard_S3Integration(t *testing.T) {
	if os.Getenv("WEFT_TEST_S3") == "" {
		// Try to reach LocalStack.
		cfg, err := config.LoadDefaultConfig(context.Background(),
			config.WithRegion("us-east-1"),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s3TestAccessKey, s3TestSecretKey, "")),
		)
		if err != nil {
			t.Skip("skipping S3 test: no AWS config")
		}
		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(s3TestEndpoint)
			o.UsePathStyle = true
		})
		_, err = client.ListBuckets(context.Background(), &s3.ListBucketsInput{})
		if err != nil {
			t.Skip("skipping S3 test: LocalStack not reachable at " + s3TestEndpoint)
		}
	}

	// Create bucket.
	cfg, _ := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(s3TestAccessKey, s3TestSecretKey, "")),
	)
	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(s3TestEndpoint)
		o.UsePathStyle = true
	})
	s3Client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(s3TestBucket),
	})

	// Ingest test data.
	ds, err := shard.NewDuckDBShard("s3-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{Raw: "s3 test row 1", Namespace: "vimgym", Level: "ERROR", CustomerId: 1, TsUnixNs: coldT0.Add(1 * time.Second).UnixNano()},
		{Raw: "s3 test row 2", Namespace: "vimgym", Level: "INFO", CustomerId: 1, TsUnixNs: coldT0.Add(2 * time.Second).UnixNano()},
		{Raw: "s3 test row 3", Namespace: "callbox", Level: "WARN", CustomerId: 1, TsUnixNs: coldT0.Add(3 * time.Second).UnixNano()},
	}
	if err := ds.Ingest(context.Background(), rows, nil); err != nil {
		t.Fatal(err)
	}

	// Export to local Parquet.
	localPath := filepath.Join(t.TempDir(), "s3test.parquet")
	if err := ds.ExportToParquet(localPath); err != nil {
		t.Fatal(err)
	}

	// Upload to S3.
	s3Key := "test/shard.shard-s3-test.parquet"
	f, err := os.Open(localPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	_, err = s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(s3TestBucket),
		Key:    aws.String(s3Key),
		Body:   f,
	})
	if err != nil {
		t.Fatalf("upload to S3: %v", err)
	}
	t.Logf("uploaded to s3://%s/%s", s3TestBucket, s3Key)

	// Create ColdShard pointing at S3 (LocalStack).
	s3Path := fmt.Sprintf("s3://%s/%s", s3TestBucket, s3Key)
	cs, err := shard.NewColdShard("cold-s3", []string{"vimgym", "callbox"}, s3Path, "", &shard.S3Config{
		Endpoint:  "localhost:4566",
		AccessKey: s3TestAccessKey,
		SecretKey: s3TestSecretKey,
		Region:    "us-east-1",
		UseSSL:    false,
	})

	if err != nil {
		t.Fatalf("create cold shard.shard with S3: %v", err)
	}
	defer cs.Close()

	// Query all rows.
	result, err := cs.Query(context.Background(), "SELECT raw, ts FROM logs WHERE customer_id = 1")
	if err != nil {
		t.Fatalf("query from S3: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 rows from S3, got %d", len(result))
	}
	t.Logf("queried %d rows from S3 Parquet", len(result))

	// Query with filter.
	result, err = cs.Query(context.Background(), "SELECT raw, ts FROM logs WHERE namespace = 'vimgym' AND customer_id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 vimgym rows from S3, got %d", len(result))
	}

	// Query with level filter.
	result, err = cs.Query(context.Background(), "SELECT raw, ts FROM logs WHERE level = 'ERROR' AND customer_id = 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 ERROR row from S3, got %d", len(result))
	}
	if result[0].Raw != "s3 test row 1" {
		t.Fatalf("expected 's3 test row 1', got '%s'", result[0].Raw)
	}
}

var coldT0 = time.Date(2026, 3, 28, 10, 0, 0, 0, time.UTC)
