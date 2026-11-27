package s3

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	awss3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/w3ft-net/weft/backends/dlq"
	"github.com/w3ft-net/weft/backends/dlq/dlqstoretest"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// fakeBucket is an in-memory bucket.
type fakeBucket struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (b *fakeBucket) PutObject(_ context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.objects == nil {
		b.objects = map[string][]byte{}
	}
	b.objects[*in.Key] = body
	return &awss3.PutObjectOutput{}, nil
}

func (b *fakeBucket) ListObjectsV2(_ context.Context, in *awss3.ListObjectsV2Input, _ ...func(*awss3.Options)) (*awss3.ListObjectsV2Output, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := &awss3.ListObjectsV2Output{}
	for k := range b.objects {
		if strings.HasPrefix(k, *in.Prefix) {
			out.Contents = append(out.Contents, awss3types.Object{Key: aws.String(k)})
		}
	}
	return out, nil
}

func (b *fakeBucket) GetObject(_ context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	body, ok := b.objects[*in.Key]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s", *in.Key)
	}
	return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (b *fakeBucket) DeleteObject(_ context.Context, in *awss3.DeleteObjectInput, _ ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, *in.Key)
	return &awss3.DeleteObjectOutput{}, nil
}

func (b *fakeBucket) keys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for k := range b.objects {
		out = append(out, k)
	}
	return out
}

func open() (*DLQ, *fakeBucket) {
	b := &fakeBucket{}
	return &DLQ{client: b, bucket: "test-bucket", prefix: "dlq/"}, b
}

func TestConformance(t *testing.T) {
	dlqstoretest.Run(t, func(t *testing.T) dlq.Store { s, _ := open(); return s })
}

// The object is named dlq/<date>/<ns>.jsonl.gz, and the ref is that key.
func TestWrite_KeyIsDatedUnderThePrefix(t *testing.T) {
	s, b := open()
	ref, err := s.Write(context.Background(), shapetest.Added(1), []*pb.LogRow{{Raw: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	keys := b.keys()
	if len(keys) != 1 || ref != keys[0] {
		t.Fatalf("ref %q, objects %v", ref, keys)
	}
	if !strings.HasPrefix(keys[0], "dlq/") || !strings.HasSuffix(keys[0], ".jsonl.gz") {
		t.Errorf("key %q should be dlq/<date>/<ns>.jsonl.gz", keys[0])
	}
}

// An object it cannot decode stays in the bucket, is reported, and the others
// still replay.
func TestReplay_AnObjectItCannotDecodeStaysAndTheOthersReplay(t *testing.T) {
	s, b := open()
	ctx := context.Background()
	b.PutObject(ctx, &awss3.PutObjectInput{Key: aws.String("dlq/2026-01-01/1.jsonl.gz"), Body: strings.NewReader("not gzip")})
	if _, err := s.Write(ctx, shapetest.Added(1), []*pb.LogRow{{Raw: "good"}}); err != nil {
		t.Fatal(err)
	}
	var got []dlq.Entry
	n, err := s.Replay(ctx, func(e []dlq.Entry) { got = append(got, e...) })
	if err == nil || n != 1 || len(got) != 1 || got[0].Raw != "good" {
		t.Fatalf("Replay = %d, %v, sink saw %v", n, err, got)
	}
	if keys := b.keys(); len(keys) != 1 || keys[0] != "dlq/2026-01-01/1.jsonl.gz" {
		t.Errorf("objects left = %v, want the undecodable one", keys)
	}
}

func TestNew_RefusesNoBucket(t *testing.T) {
	if _, err := NewDLQ(context.Background(), "us-east-2", ""); err == nil {
		t.Fatal("New with no bucket succeeded")
	}
}
