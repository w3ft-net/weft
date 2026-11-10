package coordinator

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/graybeardcorp/weft/store"
	"github.com/graybeardcorp/weft/tenancy/hosted"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/graybeardcorp/weft/compute"
	computesvc "github.com/graybeardcorp/weft/compute/service"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	shardsvc "github.com/graybeardcorp/weft/shard/service"
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
	// The fixture rows belong to customer id 1, store.TestOrg's fixed id for "kevlar".
	if err := ds0.Ingest(context.Background(), DevShardRows("shard-0", 1), nil); err != nil {
		t.Fatal(err)
	}
	shard0Addr := startShardServer(t, ds0)

	// --- Shard 1 ---
	ds1, err := shard.NewDuckDBShard("shard-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ds1.Ingest(context.Background(), DevShardRows("shard-1", 1), nil); err != nil {
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

	pool, err := compute.NewClient(computeAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })

	apps := []string{"vimgym", "callbox"}
	c := NewCoordinator(CoordinatorConfig{Compute: pool, Store: testQueryStore(t, "kevlar", "vimgym", "callbox", "warp")})
	e0 := &logdb.Shard{
		ID:          "shard-0",
		WindowStart: DevT0, WindowEnd: DevT1,
		State: logdb.StateIngesting, Live: gs0, Addr: shard0Addr,
	}
	e1 := &logdb.Shard{
		ID:          "shard-1",
		WindowStart: DevT1, WindowEnd: DevT2,
		State: logdb.StateIngesting, Live: gs1, Addr: shard1Addr,
	}
	logdb.SeedShard(c.Provisioner, e0)
	logdb.SeedShard(c.Provisioner, e1)
	// SeedShard skips christen, so stamp shard_services (+ the
	// shard_records parent) the service-level read router needs — same
	// stand-in addInProcessShard does for the in-process fixtures.
	stampShardServicesFromApps(t, c, e0, apps)
	stampShardServicesFromApps(t, c, e1, apps)
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
	srv := grpc.NewServer()
	pb.RegisterWeftComputeServer(srv, computesvc.NewServer(&compute.Engine{}))
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
	res, err := c.Execute(context.Background(), `cat vimgym.log | awk 'LEVEL == "ERROR"'`, grpcFullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	rows := resD.Rows
	for _, r := range rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
		if !strings.Contains(r.Raw, "ERROR") {
			t.Errorf("expected raw to contain 'ERROR', got %q", r.Raw)
		}
	}
	if len(rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(rows))
	}
}

func TestGRPC_GrepTimeout(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Execute(context.Background(), `cat warp.log | grep timeout`, grpcFullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if len(resD.Rows) != 5 {
		t.Errorf("expected 5 rows, got %d", len(resD.Rows))
	}
}

func TestGRPC_JqSelect(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Execute(context.Background(), `cat vimgym.log | jq 'select(.status == 500)'`, grpcFullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	for _, r := range resD.Rows {
		if !strings.Contains(r.Raw, "vimgym") {
			t.Errorf("expected raw to contain 'vimgym', got %q", r.Raw)
		}
	}
	if len(resD.Rows) != 3 {
		t.Errorf("expected 3 rows, got %d", len(resD.Rows))
	}
}

func TestGRPC_HeadLimit(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Execute(context.Background(), `head -3 vimgym.log`, grpcFullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if len(resD.Rows) > 3 {
		t.Errorf("expected at most 3 rows, got %d", len(resD.Rows))
	}
}

func TestGRPC_CustomerIsolation(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Execute(context.Background(), `cat vimgym.log`, grpcFullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	for _, r := range resD.Rows {
		if strings.Contains(r.Raw, "should not appear") {
			t.Fatal("customer isolation broken: acme row leaked")
		}
	}
}

func TestGRPC_ComputeSort(t *testing.T) {
	c := setupGRPCCoordinator(t)
	res, err := c.Execute(context.Background(), `cat vimgym.log | awk 'LEVEL == "ERROR"' | sort`, grpcFullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if resD.Text == "" {
		t.Fatal("expected text output")
	}
	lines := grpcNonEmptyLines(resD.Text)
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
	res, err := c.Execute(context.Background(), `cat warp.log | grep timeout | wc -l`, grpcFullRange, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if strings.TrimSpace(resD.Text) != "5" {
		t.Errorf("expected 5, got %q", strings.TrimSpace(resD.Text))
	}
}

func TestGRPC_TimeRangeScoping(t *testing.T) {
	c := setupGRPCCoordinator(t)
	tr := TimeRange{Start: DevT0, End: DevT1}
	res, err := c.Execute(context.Background(), `cat vimgym.log | awk 'LEVEL == "ERROR"'`, tr, hosted.Actor{Org: store.TestOrgID("kevlar"), User: ""})
	if err != nil {
		t.Fatal(err)
	}
	resD := drainExec(t, res)
	if len(resD.Rows) != 3 {
		t.Errorf("expected 3 from shard 0, got %d", len(resD.Rows))
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
