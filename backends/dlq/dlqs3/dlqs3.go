// Package dlqs3 is the hosted build's dead-letter store: each batch an object
// in an S3 bucket, gzipped JSON lines under dlq/<date>/.
package dlqs3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/w3ft-net/weft/backends/dlq"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

var _ dlq.Store = (*Store)(nil)

// api is the slice of *s3.Client that Store uses; *s3.Client satisfies it, and
// the tests substitute a fake bucket.
type api interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// Store keeps batches in one bucket.
type Store struct {
	client api
	bucket string
	prefix string
}

// New is a store over the bucket, which may be given as s3://name[/...]. The
// credentials are the AWS default chain's.
func New(ctx context.Context, region, bucket string) (*Store, error) {
	bucket = strings.TrimPrefix(bucket, "s3://")
	if i := strings.Index(bucket, "/"); i >= 0 {
		bucket = bucket[:i]
	}
	if bucket == "" {
		return nil, errors.New("dlqs3: no bucket")
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("dlqs3: load AWS config: %w", err)
	}
	return &Store{client: s3.NewFromConfig(cfg), bucket: bucket, prefix: "dlq/"}, nil
}

func (s *Store) Write(ctx context.Context, added shape.Added, rows []*pb.LogRow) (string, error) {
	if len(rows) == 0 {
		return "", nil
	}
	body, err := dlq.Encode(added, rows)
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	key := fmt.Sprintf("%s%s/%d.jsonl.gz", s.prefix, now.Format("2006-01-02"), now.UnixNano())
	if _, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(body)}); err != nil {
		return "", fmt.Errorf("write s3://%s/%s: %w", s.bucket, key, err)
	}
	return key, nil
}

func (s *Store) Replay(ctx context.Context, sink func([]dlq.Entry)) (int, error) {
	var (
		total int
		errs  []error
	)
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(s.prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return total, errors.Join(append(errs, fmt.Errorf("list dlq: %w", err))...)
		}
		for _, obj := range page.Contents {
			entries, err := s.read(ctx, *obj.Key)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			sink(entries)
			total += len(entries)
			if _, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: obj.Key}); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", *obj.Key, err))
			}
		}
	}
	return total, errors.Join(errs...)
}

func (s *Store) read(ctx context.Context, key string) ([]dlq.Entry, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()
	entries, err := dlq.Decode(out.Body)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", key, err)
	}
	return entries, nil
}
