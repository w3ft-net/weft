package coordinator

import (
	"fmt"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kevlarcon/weft/compute"
	computesvc "github.com/kevlarcon/weft/compute/service"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	shardsvc "github.com/kevlarcon/weft/shard/service"
)

// setupGRPCCoordinator starts two shard gRPC servers and one compute gRPC
// server in-process, then returns a Coordinator wired to them via gRPC clients.
func setupGRPCCoordinator(t *testing.T) *Coordinator {
	t.Helper()

	// --- Shard 0 ---
	ds0, err := shard.NewDuckDBShard("shard-0", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ds0.Ingest(DevShardRows("shard-0")); err != nil {
		t.Fatal(err)
	}
	shard0Addr := startShardServer(t, ds0)

	// --- Shard 1 ---
	ds1, err := shard.NewDuckDBShard("shard-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ds1.Ingest(DevShardRows("shard-1")); err != nil {
		t.Fatal(err)
	}
	shard1Addr := startShardServer(t, ds1)

	// --- Compute worker ---
	computeAddr := startComputeServer(t)

	// --- Connect gRPC clients ---
	gs0, err := shard.NewGRPCShard("shard-0", shard0Addr, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gs0.Close() })

	gs1, err := shard.NewGRPCShard("shard-1", shard1Addr, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gs1.Close() })

	pool, err := compute.NewGRPCComputePool(computeAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })

	apps := []string{"vimgym", "callbox"}
	c := NewCoordinator(CoordinatorConfig{Pool: pool, Store: testQueryStore(t, "kevlar", "vimgym", "callbox", "warp")})
	c.Registry.Add(&ShardEntry{
		ID: "shard-0", Apps: apps,
		WindowStart: DevT0, WindowEnd: DevT1,
		State: StateIngesting, Shard: gs0,
	})
	c.Registry.Add(&ShardEntry{
		ID: "shard-1", Apps: apps,
		WindowStart: DevT1, WindowEnd: DevT2,
		State: StateIngesting, Shard: gs1,
	})
	return c
}

func startShardServer(t *testing.T, ds *shard.DuckDBShard) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterShardQueryServer(srv, shardsvc.NewServer(ds))
	go srv.Serve(lis)
	t.Cleanup(func() {
		srv.GracefulStop()
		ds.Close()
	})
	return lis.Addr().String()
}

func startComputeServer(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pool := &compute.LocalComputePool{Parallelism: 2}
	srv := grpc.NewServer()
	pb.RegisterWeftComputeServer(srv, computesvc.NewServer(pool))
	go srv.Serve(lis)
	t.Cleanup(func() { srv.GracefulStop() })
	return lis.Addr().String()
}

// ---------------------------------------------------------------------------
// Integration tests — same queries as the in-process tests, over gRPC
// ---------------------------------------------------------------------------

var grpcFullRange = TimeRange{Start: DevT0, End: DevT2}

func TestGRPC_AwkLevelFilter(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Query(`cat vimgym.log | awk '$LEVEL == "ERROR"'`, grpcFullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	rows := res.Rows
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q (line %d)", r.Raw, r.LineNum)
		}
		if !strings.Contains(r.Raw, "ERROR") {
			t.Errorf("expected raw to contain 'ERROR', got %q (line %d)", r.Raw, r.LineNum)
		}
	}
	if len(rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(rows))
	}
}

func TestGRPC_GrepTimeout(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Query(`cat warp.log | grep timeout`, grpcFullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(res.Rows))
	}
}

func TestGRPC_JqSelect(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Query(`cat vimgym.log | jq 'select(.status == 500)'`, grpcFullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
	}
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 rows, got %d", len(res.Rows))
	}
}

func TestGRPC_HeadLimit(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Query(`head -3 vimgym.log`, grpcFullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) > 3 {
		t.Errorf("expected at most 3 rows, got %d", len(res.Rows))
	}
}

func TestGRPC_CustomerIsolation(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Query(`cat vimgym.log`, grpcFullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res.Rows {
		if strings.Contains(r.Raw, "should not appear") {
			t.Fatalf("customer isolation broken: acme row leaked on line %d", r.LineNum)
		}
	}
}

func TestGRPC_ComputeSort(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Query(`cat vimgym.log | awk '$LEVEL == "ERROR"' | sort`, grpcFullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Text == "" {
		t.Fatal("expected text output")
	}
	lines := grpcNonEmptyLines(res.Text)
	if len(lines) != 5 {
		t.Errorf("expected 5 lines, got %d", len(lines))
	}
	for i := 1; i < len(lines); i++ {
		if lines[i] < lines[i-1] {
			t.Errorf("not sorted at %d", i)
		}
	}
}

func TestGRPC_ComputeWcCount(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Query(`cat warp.log | grep timeout | wc -l`, grpcFullRange, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(res.Text) != "5" {
		t.Errorf("expected 5, got %q", strings.TrimSpace(res.Text))
	}
}

func TestGRPC_TimeRangeScoping(t *testing.T) {
	c := setupGRPCCoordinator(t)
	tr := TimeRange{Start: DevT0, End: DevT1}
	res, err := c.Query(`cat vimgym.log | awk '$LEVEL == "ERROR"'`, tr, "kevlar", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 3 {
		t.Errorf("expected 3 from shard 0, got %d", len(res.Rows))
	}
}

func grpcNonEmptyLines(text string) []string {
	var out []string
	for _, l := range strings.Split(text, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// Suppress unused import warnings.
var _ = fmt.Sprintf
var _ = insecure.NewCredentials
