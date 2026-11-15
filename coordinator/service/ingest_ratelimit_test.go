package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"

	weftcfg "github.com/w3ft-net/weft/config"
	"github.com/w3ft-net/weft/coordinator"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// TestIngestStream_PerTokenRateLimit_ThrottlesButDeliversAll pins the
// end-to-end wiring for the per-token rate limit (see
// docs/ingestion-syslog.md, "Rate limiting"): a token pushing rows
// faster than its configured budget is slowed down, not disconnected or
// dropped, and the auth.TokenID → runtime-config plumbing actually
// reaches the limiter.
func TestIngestStream_PerTokenRateLimit_ThrottlesButDeliversAll(t *testing.T) {
	env := newSvcEnv(t)
	rawTok := env.agentToken("ratelimit-test", "rl-agent")

	newRow := func() *pb.LogRow { return &pb.LogRow{Raw: strings.Repeat("x", 200)} }
	rowSize := int64(proto.Size(newRow()))

	// Burst covers exactly one row; refill rate replenishes one row's
	// worth every 200ms. Row 1 spends the whole burst, so rows 2 and 3
	// must each wait ~200ms.
	cfg := weftcfg.Defaults()
	cfg.IngestTokenBytesPerSec = rowSize * 5
	cfg.IngestTokenBurstBytes = rowSize
	var cfgPtr atomic.Pointer[weftcfg.Runtime]
	cfgPtr.Store(cfg)

	srv := env.server(coordinator.CoordinatorConfig{}, &Server{CfgPtr: &cfgPtr})
	coord := srv.Coord

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+rawTok))

	// Distinct row objects: IngestStream stamps Customer/Namespace/Kind
	// onto each row in place, so reusing one pointer would grow its
	// encoded size across iterations and desync it from rowSize above.
	st := &fakeIngestStream{ctx: ctx, rows: []*pb.LogRow{newRow(), newRow(), newRow()}}
	start := time.Now()
	if err := srv.IngestStream(st); err != nil {
		t.Fatalf("IngestStream: %v", err)
	}
	elapsed := time.Since(start)

	// A floor well below the ~400ms (2 x 200ms) expected wait still
	// distinguishes "throttled" from "instant" (which completes in
	// low single-digit milliseconds), while tolerating CI jitter.
	if elapsed < 250*time.Millisecond {
		t.Fatalf("IngestStream completed in %v; want >= 250ms if the per-token rate limit is actually throttling", elapsed)
	}

	// No data lost: all 3 rows still made it to the ring buffer despite
	// the throttling.
	got := coord.Ingest.Drain(10)
	if len(got) != 3 {
		t.Fatalf("ring buffer has %d rows, want 3 — throttling must delay rows, not drop them", len(got))
	}
}

// TestIngestStream_PerTokenRateLimit_Disabled pins that bytesPerSec <= 0
// (the zero-value default an operator sets in SSM to turn the limiter
// off) never delays ingestion.
func TestIngestStream_PerTokenRateLimit_Disabled(t *testing.T) {
	env := newSvcEnv(t)
	rawTok := env.agentToken("ratelimit-disabled-test", "rl-agent-2")

	cfg := weftcfg.Defaults()
	cfg.IngestTokenBytesPerSec = 0
	cfg.IngestTokenBurstBytes = 0
	var cfgPtr atomic.Pointer[weftcfg.Runtime]
	cfgPtr.Store(cfg)

	srv := env.server(coordinator.CoordinatorConfig{}, &Server{CfgPtr: &cfgPtr})

	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer "+rawTok))

	newRow := func() *pb.LogRow { return &pb.LogRow{Raw: strings.Repeat("x", 200)} }
	st := &fakeIngestStream{ctx: ctx, rows: []*pb.LogRow{newRow(), newRow(), newRow(), newRow(), newRow()}}

	start := time.Now()
	if err := srv.IngestStream(st); err != nil {
		t.Fatalf("IngestStream: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("disabled rate limit should not delay ingestion, took %v", elapsed)
	}
}
