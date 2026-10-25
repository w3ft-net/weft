package coordinator

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	weftsdk "github.com/w3ft-net/weft-go"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// s3API is the slice of *s3.Client that DLQ actually uses.
// Lowercased so it's not part of the package's public surface — its
// only consumer outside dlq.go is the in-package test that
// substitutes a fake bucket. *s3.Client satisfies it natively.
type s3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// DLQ writes failed ingest batches to S3 as gzipped JSON-lines.
// Replay feeds them back through the ingest path.
type DLQ struct {
	client s3API
	bucket string
	prefix string // e.g. "dlq/"
}

// NewDLQ creates a DLQ that writes to the given S3 bucket.
// Returns nil if bucket is empty (DLQ disabled).
func NewDLQ(region, bucket string) *DLQ {
	if bucket == "" {
		return nil
	}
	// Strip s3:// prefix if present.
	bucket = strings.TrimPrefix(bucket, "s3://")
	if idx := strings.Index(bucket, "/"); idx >= 0 {
		bucket = bucket[:idx]
	}

	cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
	if err != nil {
		weftsdk.Errorf(context.Background(), "dlq: failed to load AWS config: %v", err)
		return nil
	}

	return &DLQ{
		client: s3.NewFromConfig(cfg),
		bucket: bucket,
		prefix: "dlq/",
	}
}

// Write uploads a failed batch to S3 as gzipped JSON-lines, one protojson
// LogRow per line, so a replay restores every field.
// Returns the S3 key path, or "" on failure.
func (d *DLQ) Write(rows []*pb.LogRow) string {
	if d == nil || len(rows) == 0 {
		return ""
	}

	now := time.Now().UTC()
	key := fmt.Sprintf("%s%s/%d.jsonl.gz", d.prefix, now.Format("2006-01-02"), now.UnixNano())

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)

	for _, r := range rows {
		line, err := protojson.Marshal(r)
		if err != nil {
			weftsdk.Errorf(context.Background(), "dlq: marshal row: %v", err)
			return ""
		}
		gz.Write(line)
		gz.Write([]byte{'\n'})
	}
	gz.Close()

	_, err := d.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(buf.Bytes()),
	})
	if err != nil {
		weftsdk.Errorf(context.Background(), "dlq: failed to write %d rows to s3://%s/%s: %v", len(rows), d.bucket, key, err)
		return ""
	}

	weftsdk.Infof(context.Background(), "dlq: wrote %d rows to s3://%s/%s", len(rows), d.bucket, key)
	return key
}

// Replay feeds every row in the DLQ to sink, one object at a time, and
// deletes an object only after sink has taken its rows: a crash in between
// delivers the object twice rather than never. Returns the rows delivered.
// An object that can't be read or decoded stays in the bucket and is
// reported in the error; the others still replay.
func (d *DLQ) Replay(ctx context.Context, sink func([]*pb.LogRow)) (int, error) {
	if d == nil {
		return 0, nil
	}
	var (
		total int
		errs  []error
	)
	pages := s3.NewListObjectsV2Paginator(d.client, &s3.ListObjectsV2Input{Bucket: aws.String(d.bucket), Prefix: aws.String(d.prefix)})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return total, errors.Join(append(errs, fmt.Errorf("list dlq: %w", err))...)
		}
		for _, obj := range page.Contents {
			rows, err := d.readObject(ctx, *obj.Key)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			sink(rows)
			total += len(rows)
			if _, err := d.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(d.bucket), Key: obj.Key}); err != nil {
				errs = append(errs, fmt.Errorf("delete %s: %w", *obj.Key, err))
			}
		}
	}
	return total, errors.Join(errs...)
}

func (d *DLQ) readObject(ctx context.Context, key string) ([]*pb.LogRow, error) {
	out, err := d.client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(d.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	defer out.Body.Close()
	gz, err := gzip.NewReader(out.Body)
	if err != nil {
		return nil, fmt.Errorf("gunzip %s: %w", key, err)
	}
	defer gz.Close()
	var rows []*pb.LogRow
	dec := json.NewDecoder(gz)
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			return rows, nil
		} else if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		row := &pb.LogRow{}
		if err := protojson.Unmarshal(raw, row); err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		rows = append(rows, row)
	}
}
