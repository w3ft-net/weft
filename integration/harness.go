// Package integration provides the shared test harness for weft integration tests.
// TestMain handles docker-compose lifecycle, MinIO bucket creation, and env var setup.
//
// Run all integration tests:
//
//	go test ./integration/ -v -timeout=5m
package integration

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	MinioEndpoint  = "localhost:9050"
	MinioAccessKey = "minioadmin"
	MinioSecretKey = "minioadmin"
	ColdBucket     = "weft-cold"
	CoordAddr      = "localhost:9020"
)

// Setup starts all required infrastructure and sets environment variables.
// Call from TestMain. Returns a cleanup function.
func Setup() func() {
	// Set env vars so S3ConfigFromEnv() works everywhere.
	os.Setenv("WEFT_S3_ENDPOINT", MinioEndpoint)
	os.Setenv("WEFT_S3_ACCESS_KEY", MinioAccessKey)
	os.Setenv("WEFT_S3_SECRET_KEY", MinioSecretKey)
	os.Setenv("WEFT_S3_REGION", "us-east-1")
	os.Setenv("WEFT_S3_USE_SSL", "false")

	// Start docker-compose stack.
	log.Println("integration: starting docker-compose stack...")
	up := exec.Command("docker", "compose", "up", "-d", "--build", "--wait")
	up.Dir = ".."
	up.Stdout = os.Stdout
	up.Stderr = os.Stderr
	if err := up.Run(); err != nil {
		log.Fatalf("docker compose up: %v", err)
	}

	// Wait for MinIO and create bucket.
	log.Println("integration: configuring MinIO...")
	if err := ensureMinioBucket(); err != nil {
		log.Fatalf("MinIO setup: %v", err)
	}

	log.Println("integration: ready")

	return func() {
		// Leave stack running for fast re-runs.
		// Use `docker compose down` to clean up manually.
	}
}

func ensureMinioBucket() error {
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(MinioAccessKey, MinioSecretKey, "")),
	)
	if err != nil {
		return err
	}

	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(fmt.Sprintf("http://%s", MinioEndpoint))
		o.UsePathStyle = true
	})

	// Retry until MinIO is ready.
	for i := 0; i < 30; i++ {
		_, err := client.ListBuckets(context.Background(), &s3.ListBucketsInput{})
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	// Create bucket (ignore if exists).
	client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(ColdBucket),
	})

	return nil
}
