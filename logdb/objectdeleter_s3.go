package logdb

import (
	"context"
	"fmt"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// This file is the S3 ObjectDeleter; it moves to ext/storage/s3.

// S3ObjectDeleter is the prod ObjectDeleter. A single-file path is one
// DeleteObject; a hive glob ("s3://b/dir/**/*.parquet") is a
// ListObjectsV2 over the prefix + a batched DeleteObjects. DeleteObject is
// idempotent on a missing key, so retries are safe.
type S3ObjectDeleter struct {
	client *s3.Client
	// timeout bounds each S3 call.
	timeout time.Duration
}

// NewS3ObjectDeleter builds a deleter from the default credential chain
// (IAM role on the coordinator's instance).
func NewS3ObjectDeleter(ctx context.Context, region string) (*S3ObjectDeleter, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return &S3ObjectDeleter{client: s3.NewFromConfig(cfg), timeout: time.Minute}, nil
}

func (d *S3ObjectDeleter) DeleteParquet(path string) error {
	bucket, key, err := parseS3Path(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
	defer cancel()

	if i := strings.Index(key, "/**/"); i >= 0 {
		return d.deletePrefix(ctx, bucket, key[:i+1]) // include trailing slash
	}
	_, err = d.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key})
	return err
}

// deletePrefix lists and batch-deletes every object under prefix — the
// hive-partitioned archive layout.
func (d *S3ObjectDeleter) deletePrefix(ctx context.Context, bucket, prefix string) error {
	p := s3.NewListObjectsV2Paginator(d.client, &s3.ListObjectsV2Input{Bucket: &bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list %s/%s: %w", bucket, prefix, err)
		}
		if len(page.Contents) == 0 {
			continue
		}
		ids := make([]s3types.ObjectIdentifier, len(page.Contents))
		for i, obj := range page.Contents {
			ids[i] = s3types.ObjectIdentifier{Key: obj.Key}
		}
		if _, err := d.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &bucket,
			Delete: &s3types.Delete{Objects: ids},
		}); err != nil {
			return fmt.Errorf("delete batch under %s/%s: %w", bucket, prefix, err)
		}
	}
	return nil
}

// parseS3Path splits "s3://bucket/key..." into bucket and key.
func parseS3Path(path string) (bucket, key string, err error) {
	rest, ok := strings.CutPrefix(path, "s3://")
	if !ok {
		return "", "", fmt.Errorf("not an s3 path: %q", path)
	}
	bucket, key, ok = strings.Cut(rest, "/")
	if !ok || bucket == "" || key == "" {
		return "", "", fmt.Errorf("malformed s3 path: %q", path)
	}
	return bucket, key, nil
}
