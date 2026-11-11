package coordinator

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/graybeardcorp/weft/compute"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// recordingPutter is an in-memory s3PutObjectAPI that captures each
// PutObject call. The DLQ's internal client field is unexported and
// accepts this interface (defined in dlq.go), so same-package tests
// can substitute it without touching the public surface.
type recordingPutter struct {
	mu      sync.Mutex
	calls   []recordedPut
	objects map[string][]byte // live objects: put, minus deleted
}

type recordedPut struct {
	Key  string
	Body []byte
}

func (r *recordingPutter) PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	// Drain Body now — it's a one-shot reader, and the rest of the
	// test wants the bytes that were on their way to S3.
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.calls = append(r.calls, recordedPut{Key: *in.Key, Body: body})
	if r.objects == nil {
		r.objects = map[string][]byte{}
	}
	r.objects[*in.Key] = body
	r.mu.Unlock()
	return &s3.PutObjectOutput{}, nil
}

func (r *recordingPutter) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := &s3.ListObjectsV2Output{}
	for k := range r.objects {
		if strings.HasPrefix(k, *in.Prefix) {
			out.Contents = append(out.Contents, s3types.Object{Key: aws.String(k)})
		}
	}
	return out, nil
}

func (r *recordingPutter) GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	body, ok := r.objects[*in.Key]
	if !ok {
		return nil, fmt.Errorf("NoSuchKey: %s", *in.Key)
	}
	return &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func (r *recordingPutter) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.objects, *in.Key)
	return &s3.DeleteObjectOutput{}, nil
}

// Live returns how many objects are still in the fake bucket.
func (r *recordingPutter) Live() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.objects)
}

func (r *recordingPutter) Calls() []recordedPut {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedPut, len(r.calls))
	copy(out, r.calls)
	return out
}

// testDLQ builds a DLQ wired to the recording fake. Construction via
// struct literal works because dlq_test.go lives in the coordinator
// package; the production NewDLQ is unsuitable here (it dials S3).
func testDLQ() (*DLQ, *recordingPutter) {
	p := &recordingPutter{}
	return &DLQ{client: p, bucket: "test-bucket", prefix: "dlq/"}, p
}

// dlqFailingShard is a shard.Shard whose Ingest always errors. Kept
// local to this test file — its only consumer is the DLQ drain
// coverage below.
type dlqFailingShard struct{ id string }

func (s *dlqFailingShard) ID() string { return s.id }
func (s *dlqFailingShard) Query(context.Context, string, ...any) ([]*pb.ResultRow, error) {
	return nil, nil
}
func (s *dlqFailingShard) QueryToSink(context.Context, string, []any, shard.RowSink) {}
func (s *dlqFailingShard) Archive(_ context.Context, _ string) (string, []shard.CustomerStat, error) {
	return "", nil, nil
}
func (s *dlqFailingShard) Close() error { return nil }
func (s *dlqFailingShard) Ingest(_ context.Context, _ []*pb.LogRow, _ shard.IngestRecorder) error {
	return fmt.Errorf("shard %s down", s.id)
}

func dlqTestRows(app string, n int) []*pb.LogRow {
	rows := make([]*pb.LogRow, n)
	now := time.Now().UnixNano()
	ns, svc, kind := shard.SplitApp(app)
	for i := range rows {
		rows[i] = &pb.LogRow{
			Namespace:  ns,
			Service:    svc,
			Kind:       kind,
			CustomerId: 1,
			Raw:        fmt.Sprintf("row-%d", i),
			TsUnixNs:   now,
		}
	}
	return rows
}

// TestDLQ_Write_PayloadShape pins the on-wire format of a DLQ write:
// the key is timestamped under the prefix, the body is gzip, and each
// decompressed line is a JSON record with the fields the recovery
// scanner expects.
func TestDLQ_Write_PayloadShape(t *testing.T) {
	d, putter := testDLQ()
	rows := dlqTestRows("weft/coordinator", 3)

	key := d.Write(rows)
	if key == "" {
		t.Fatal("Write returned empty key on success")
	}
	calls := putter.Calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 PutObject call, got %d", len(calls))
	}
	put := calls[0]
	if put.Key != key {
		t.Errorf("key mismatch: returned %q, PutObject got %q", key, put.Key)
	}
	if !strings.HasPrefix(put.Key, "dlq/") || !strings.HasSuffix(put.Key, ".jsonl.gz") {
		t.Errorf("key %q should be dlq/<date>/<ns>.jsonl.gz", put.Key)
	}

	gz, err := gzip.NewReader(bytes.NewReader(put.Body))
	if err != nil {
		t.Fatalf("body not gzip: %v", err)
	}
	defer gz.Close()
	dec := json.NewDecoder(gz)
	var decoded []map[string]any
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		decoded = append(decoded, rec)
	}
	if len(decoded) != len(rows) {
		t.Fatalf("expected %d records, got %d", len(rows), len(decoded))
	}
	if decoded[0]["namespace"] != "weft" || decoded[0]["service"] != "coordinator" || decoded[0]["customerId"] != "1" { // protojson encodes int64 as a string
		t.Errorf("record fields wrong: %v", decoded[0])
	}
}

// TestDLQ_Write_NilSafe pins that callers (the drain loop) can blindly
// call Write on a nil receiver and an empty batch — both no-op paths.
func TestDLQ_Write_NilSafe(t *testing.T) {
	var d *DLQ
	if got := d.Write(dlqTestRows("x", 1)); got != "" {
		t.Errorf("nil DLQ.Write should return empty, got %q", got)
	}
	d2, putter := testDLQ()
	if got := d2.Write(nil); got != "" {
		t.Errorf("empty rows should return empty, got %q", got)
	}
	if calls := putter.Calls(); len(calls) != 0 {
		t.Errorf("empty rows should not PUT, got %d calls", len(calls))
	}
}

// TestDrain_NoHealthyShards_RoutesToDLQ verifies the drain loop's
// "no candidate at all" branch: with the registry empty, every batch
// pulled off the ring buffer lands in the DLQ.
func TestDrain_NoHealthyShards_RoutesToDLQ(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	c.Ingest.Write(dlqTestRows("weft/coordinator", 4))

	if err := waitFor(500*time.Millisecond, func() bool {
		return len(putter.Calls()) >= 1
	}); err != nil {
		t.Fatalf("DLQ never received the batch: %v", err)
	}

	put := putter.Calls()[0]
	rows := decodeDLQBody(t, put.Body)
	if len(rows) != 4 {
		t.Errorf("expected 4 rows in DLQ, got %d", len(rows))
	}
}

// TestDrain_PrimaryAndFallbackFail_RoutesToDLQ verifies the post-retry
// DLQ branch: with one failing shard (primary and fallback resolve to
// the same instance), every Ingest attempt errors and the batch lands
// in the DLQ.
func TestDrain_PrimaryAndFallbackFail_RoutesToDLQ(t *testing.T) {
	dlq, putter := testDLQ()
	c := NewCoordinator(CoordinatorConfig{Compute: &compute.Engine{}, DLQ: dlq})
	// Apps must include the row's app, otherwise IngestingShard
	// returns nil and we'd hit the "no shard owns this app" branch
	// instead of the primary+fallback-fail one we're pinning here.
	e := &logdb.Shard{
		ID:    "shard-down",
		State: logdb.StateIngesting,
		Live:  &dlqFailingShard{id: "shard-down"}, CreatedAt: time.Now(),
	}
	logdb.SeedShard(c.Provisioner, e)
	stampShardServicesFromApps(t, c, e, []string{"weft/coordinator"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.StartDrain(ctx)

	c.Ingest.Write(dlqTestRows("weft/coordinator", 5))

	if err := waitFor(500*time.Millisecond, func() bool {
		return len(putter.Calls()) >= 1
	}); err != nil {
		t.Fatalf("DLQ never received the batch: %v", err)
	}

	put := putter.Calls()[0]
	rows := decodeDLQBody(t, put.Body)
	if len(rows) != 5 {
		t.Errorf("expected 5 rows in DLQ, got %d", len(rows))
	}
	if rows[0]["service"] != "coordinator" {
		t.Errorf("wrong app in DLQ record: %v", rows[0])
	}
}

func decodeDLQBody(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("body not gzip: %v", err)
	}
	defer gz.Close()
	dec := json.NewDecoder(gz)
	var out []map[string]any
	for {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("decode: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

// waitFor polls cond until it returns true or the timeout elapses.
// Used to wait for the drain goroutine to do its work without a fixed
// sleep — keeps the test fast on a hot machine and tolerant on a busy
// one.
func waitFor(timeout time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cond() {
		return nil
	}
	return fmt.Errorf("condition not met within %s", timeout)
}

func (s *dlqFailingShard) IngestBatch(ctx context.Context, b shard.Batch, rec shard.IngestRecorder) error {
	return s.Ingest(ctx, b.Rows, rec)
}
