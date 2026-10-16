package coordinator

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/graybeardcorp/weft/internal/weftlog"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// s3PutObjectAPI is the slice of *s3.Client that DLQ actually uses.
// Lowercased so it's not part of the package's public surface — its
// only consumer outside dlq.go is the in-package test that
// substitutes a recording fake. *s3.Client satisfies it natively.
type s3PutObjectAPI interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// DLQ writes failed ingest batches to S3 as gzipped JSON-lines.
// A background scanner re-ingests them when healthy shards are available.
type DLQ struct {
	client s3PutObjectAPI
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
		weftlog.Errorf(context.Background(), "dlq: failed to load AWS config: %v", err)
		return nil
	}

	return &DLQ{
		client: s3.NewFromConfig(cfg),
		bucket: bucket,
		prefix: "dlq/",
	}
}

// Write uploads a failed batch to S3 as gzipped JSON-lines.
// Returns the S3 key path, or "" on failure.
func (d *DLQ) Write(rows []*pb.LogRow) string {
	if d == nil || len(rows) == 0 {
		return ""
	}

	now := time.Now().UTC()
	key := fmt.Sprintf("%s%s/%d.jsonl.gz", d.prefix, now.Format("2006-01-02"), now.UnixNano())

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	enc := json.NewEncoder(gz)

	for _, r := range rows {
		enc.Encode(map[string]any{
			"app":         shard.JoinApp(r.Namespace, r.Service, r.Kind),
			"customer_id": r.CustomerId,
			"raw":         r.Raw,
			"level":       r.Level,
			"instance":    r.Instance,
			"ts_ns":       r.TsUnixNs,
			"payload":     string(r.PayloadJson),
			"deployment":  r.Deployment,
			"event_id":    r.EventId,
			"line_no":     r.LineNo,
		})
	}
	gz.Close()

	_, err := d.client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(buf.Bytes()),
	})
	if err != nil {
		weftlog.Errorf(context.Background(), "dlq: failed to write %d rows to s3://%s/%s: %v", len(rows), d.bucket, key, err)
		return ""
	}

	weftlog.Infof(context.Background(), "dlq: wrote %d rows to s3://%s/%s", len(rows), d.bucket, key)
	return key
}
