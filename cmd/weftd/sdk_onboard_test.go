package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/w3ft-net/weft/ingest"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// sdkCoord stands in for a coordinator that configures an SDK-fed source from
// its final sample, as Server.SubmitSample does.
type sdkCoord struct {
	pb.WeftCoordinatorClient
	mu      sync.Mutex
	samples []*pb.SubmitSampleRequest
	schema  string
}

func (c *sdkCoord) SubmitSample(_ context.Context, req *pb.SubmitSampleRequest, _ ...grpc.CallOption) (*pb.SubmitSampleResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samples = append(c.samples, req)
	if req.SdkFed && !req.Partial {
		c.schema, _ = ingest.SDKSchema(req.Service, req.SdkFormat)
	}
	return &pb.SubmitSampleResponse{}, nil
}

func (c *sdkCoord) GetSourceStatus(_ context.Context, _ *pb.GetSourceStatusRequest, _ ...grpc.CallOption) (*pb.GetSourceStatusResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.schema != "" {
		return &pb.GetSourceStatusResponse{Status: "configured", SchemaYaml: c.schema}, nil
	}
	return &pb.GetSourceStatusResponse{Status: "unknown"}, nil
}

// A file written by the SDK is configured from its first line: weftd starts
// the tailer without collecting a full sample, and the row carries the
// header's values.
func TestOnboardAndRun_SDKFedSourceStartsFromFirstLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.log")
	writeFile(t, path, "\x1ev1\x1f1700000000000000007\x1fERROR\x1ftrace-9\x1f\x1f\x1f\x1f\x1fjson\x1f\x1e"+`{"msg":"boom"}`+"\n")

	c := &sdkCoord{}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan struct{})
	app := "sdkonboard/api/main"
	go func() {
		_ = onboardAndRun(ctx, c, "wft_x", "api", path, "", "i-1", app, func() { close(started) })
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("tailer never started; the source waited for something it should not")
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.samples) == 0 || !c.samples[len(c.samples)-1].SdkFed || c.samples[len(c.samples)-1].Partial {
		t.Fatalf("samples = %+v, want a final sdk_fed submission", c.samples)
	}
	if n := len(c.samples[len(c.samples)-1].Lines); n != 1 {
		t.Errorf("sample had %d lines, want the single first line", n)
	}

	row := buildLogRow("\x1ev1\x1f1700000000000000007\x1fERROR\x1ftrace-9\x1f\x1f\x1f\x1f\x1fjson\x1f\x1e"+`{"msg":"boom"}`, "i-1", app, "")
	if row.Level != "ERROR" || row.TraceId != "trace-9" || row.TsUnixNs != 1700000000000000007 || row.Raw != `{"msg":"boom"}` {
		t.Errorf("row = level %q trace %q ts %d raw %q", row.Level, row.TraceId, row.TsUnixNs, row.Raw)
	}
}
