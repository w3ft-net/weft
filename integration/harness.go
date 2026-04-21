// Package integration provides the shared test harness for weft integration
// tests. TestMain verifies the docker stack is running and ensures the
// MinIO cold-storage bucket exists. Migrations are the coordinator's
// responsibility (store.Open → goose.Up) — by the time the coordinator's
// /health endpoint returns 200, the schema is up to date.
//
// Typical workflow:
//
//	docker compose up -d --build
//	go test -tags integration ./integration/ -v -timeout=5m
//
// Or use ./scripts/integration-test.sh which wraps both steps.
package integration

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
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

	coordHealth    = "http://localhost:9021/health"
	postgresURL    = "postgres://weft:weft@localhost:5433/weft?sslmode=disable"
	readinessTries = 15
)

// Setup verifies the stack is up and ensures the MinIO bucket exists.
// Returns a cleanup function. Call from TestMain.
func Setup() func() {
	os.Setenv("S3_ENDPOINT", MinioEndpoint)
	os.Setenv("S3_ACCESS_KEY", MinioAccessKey)
	os.Setenv("S3_SECRET_KEY", MinioSecretKey)
	os.Setenv("S3_REGION", "us-east-1")
	os.Setenv("S3_USE_SSL", "false")

	requireStackRunning()

	if err := ensureMinioBucket(); err != nil {
		log.Fatalf("integration: MinIO setup: %v", err)
	}

	log.Println("integration: ready")

	return func() {}
}

// requireStackRunning aborts the test run with a helpful message when the
// docker compose stack isn't listening. Integration tests don't boot the
// stack anymore — that's an explicit prerequisite.
func requireStackRunning() {
	for i := 0; i < readinessTries; i++ {
		resp, err := http.Get(coordHealth)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(time.Second)
	}
	log.Fatalf(
		"integration: coordinator at %s is not reachable. "+
			"Start the stack first: `docker compose up -d --build` "+
			"(or use ./scripts/integration-test.sh).",
		coordHealth,
	)
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

	for i := 0; i < 30; i++ {
		_, err := client.ListBuckets(context.Background(), &s3.ListBucketsInput{})
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	client.CreateBucket(context.Background(), &s3.CreateBucketInput{
		Bucket: aws.String(ColdBucket),
	})

	return nil
}
