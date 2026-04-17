package shard

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

const (
	minioEndpoint  = "http://localhost:9050"
	minioAccessKey = "minioadmin"
	minioSecretKey = "minioadmin"
	minioBucket    = "weft-cold-test"
)

// TestColdShard_S3Integration tests the full flow:
// ingest → export to Parquet → upload to MinIO → ColdShard reads from S3.
//
// Requires MinIO running on localhost:9050 (docker compose up minio).
// Skipped if MinIO is not reachable.
func TestColdShard_S3Integration(t *testing.T) {
	if os.Getenv("WEFT_S3_TEST") == "" {
		// Try to reach MinIO.
		cfg, err := config.LoadDefaultConfig(context.Background(),
			config.WithRegion("us-east-1"),
			config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(minioAccessKey, minioSecretKey, "")),
		)
		if err != nil {
			t.Skip("skipping S3 test: no AWS config")
		}
		client := s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(minioEndpoint)
			o.UsePathStyle = true
		})
		_, err = client.ListBuckets(context.Background(), &s3.ListBucketsInput{})
		if err != nil {
			t.Skip("skipping S3 test: MinIO not reachable at " + minioEndpoint)
		}
	}

	// Create bucket.
	cfg, _ := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(minioAccessKey, minioSecretKey, "")),
	)
	s3Client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(minioEndpoint)
		o.UsePathStyle = true
	})
	s3Client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(minioBucket),
	})

	// Ingest test data.
	ds, err := NewDuckDBShard("s3-test", "")
	if err != nil {
		t.Fatal(err)
	}
	defer ds.Close()

	rows := []*pb.LogRow{
		{LineNum: 1, Raw: "s3 test row 1", App: "vimgym", Level: "ERROR", Customer: "kevlar", TsUnixNs: coldT0.Add(1 * time.Second).UnixNano()},
		{LineNum: 2, Raw: "s3 test row 2", App: "vimgym", Level: "INFO", Customer: "kevlar", TsUnixNs: coldT0.Add(2 * time.Second).UnixNano()},
		{LineNum: 3, Raw: "s3 test row 3", App: "callbox", Level: "WARN", Customer: "kevlar", TsUnixNs: coldT0.Add(3 * time.Second).UnixNano()},
	}
	if err := ds.Ingest(rows); err != nil {
		t.Fatal(err)
	}

	// Export to local Parquet.
	localPath := filepath.Join(t.TempDir(), "s3test.parquet")
	if err := ds.ExportToParquet(localPath); err != nil {
		t.Fatal(err)
	}

	// Upload to MinIO.
	s3Key := "test/shard-s3-test.parquet"
	f, err := os.Open(localPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	_, err = s3Client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(minioBucket),
		Key:    aws.String(s3Key),
		Body:   f,
	})
	if err != nil {
		t.Fatalf("upload to MinIO: %v", err)
	}
	t.Logf("uploaded to s3://%s/%s", minioBucket, s3Key)

	// Create ColdShard pointing at MinIO S3.
	s3Path := fmt.Sprintf("s3://%s/%s", minioBucket, s3Key)
	cs, err := NewColdShard("cold-s3", []string{"vimgym", "callbox"}, s3Path, &S3Config{
		Endpoint:  "localhost:9050",
		AccessKey: minioAccessKey,
		SecretKey: minioSecretKey,
		Region:    "us-east-1",
		UseSSL:    false,
	})

	if err != nil {
		t.Fatalf("create cold shard with S3: %v", err)
	}
	defer cs.Close()

	// Query all rows.
	result, err := cs.Query("SELECT line_num, raw FROM logs WHERE customer = 'kevlar'")
	if err != nil {
		t.Fatalf("query from S3: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 rows from S3, got %d", len(result))
	}
	t.Logf("queried %d rows from S3 Parquet", len(result))

	// Query with filter.
	result, err = cs.Query("SELECT line_num, raw FROM logs WHERE app = 'vimgym' AND customer = 'kevlar'")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 vimgym rows from S3, got %d", len(result))
	}

	// Query with level filter.
	result, err = cs.Query("SELECT line_num, raw FROM logs WHERE level = 'ERROR' AND customer = 'kevlar'")
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

