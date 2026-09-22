package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/weft/auth"
	weftcfg "github.com/graybeardcorp/weft/config"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	shardsvc "github.com/graybeardcorp/weft/shard/service"
)

func main() {
	grpcPort := flag.Int("grpc-port", 9000, "gRPC listen port")
	httpPort := flag.Int("http-port", 9001, "HTTP health listen port")
	dataDir := flag.String("data-dir", "/opt/app/data", "directory holding shard.json and shard.duckdb")
	coordAddr := flag.String("coordinator-addr", "", "coordinator gRPC address for heartbeats")
	hbInterval := flag.Duration("heartbeat-interval", 10*time.Second, "heartbeat interval")
	flag.Parse()

	if err := os.MkdirAll(*dataDir, 0755); err != nil {
		log.Fatalf("mkdir data dir %s: %v", *dataDir, err)
	}
	dbPath := filepath.Join(*dataDir, "shard.duckdb")

	// Try to start from an existing identity on disk. If shard.json is
	// missing we start in blank mode and wait for the coordinator to
	// christen us via Configure.
	var srv *shardsvc.Server
	if id, err := shardIdentityHint(*dataDir); err != nil {
		log.Fatalf("inspect shard.json: %v", err)
	} else if id != "" {
		log.Printf("found existing identity %s — opening db at %s", id, dbPath)
		srv, err = shardsvc.NewServerFromIdentity(*dataDir, dbPath)
		if err != nil {
			log.Fatalf("reopen from identity: %v", err)
		}
	} else {
		log.Printf("no shard.json in %s — starting blank, awaiting Configure", *dataDir)
		srv = shardsvc.NewBlankServer(*dataDir, dbPath)
	}

	// gRPC server with shared-token auth. Coord↔shard is an internal
	// link — both ends are trusted services, not the public internet,
	// so a single shared secret is enough instead of per-peer JWTs.
	// Both the coordinator and compute read the same SHARD_SECRET env
	// var and attach the bearer on every outbound call via
	// auth.ClientTokenUnaryInterceptor. Prod gets SHARD_SECRET from
	// Secrets Manager via load-app-env; dev sets it explicitly in
	// docker-compose.yml. No fallback — a process running with an
	// undeclared shared secret is a misconfiguration, not a default.
	shardSecret := os.Getenv("SHARD_SECRET")
	if shardSecret == "" {
		log.Fatal("no SHARD_SECRET set")
	}

	// Runtime config watcher — powers the Query timeout and feeds
	// grpc.MaxRecvMsgSize at boot. See config/runtime.go.
	appName := os.Getenv("APP_NAME")
	if appName == "" {
		appName = "weft"
	}
	// AWS SDK v2's default chain doesn't always resolve region from IMDS;
	// pass it explicitly. AWS_REGION is set by user-data on EC2.
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-2"
	}
	cfgCtx, cfgCancel := context.WithCancel(context.Background())
	defer cfgCancel()
	cfgPtr := weftcfg.Watch(cfgCtx, appName, region, 60*time.Second)
	srv.CfgPtr = cfgPtr
	bootCfg := cfgPtr.Load()

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *grpcPort))
	if err != nil {
		log.Fatalf("listen grpc: %v", err)
	}
	gs := grpc.NewServer(
		grpc.MaxRecvMsgSize(bootCfg.GRPCMaxRecvBytes),
		grpc.UnaryInterceptor(auth.SharedTokenUnaryServerInterceptor(shardSecret)),
		grpc.StreamInterceptor(auth.SharedTokenStreamServerInterceptor(shardSecret)),
	)
	pb.RegisterShardQueryServer(gs, srv)

	go func() {
		log.Printf("shard gRPC listening on :%d", *grpcPort)
		if err := gs.Serve(lis); err != nil {
			log.Printf("grpc serve error: %v", err)
		}
	}()

	// Heartbeat goroutine.
	if *coordAddr != "" {
		go runHeartbeat(*coordAddr, *grpcPort, *hbInterval, srv)
	}

	wc, _ := weftsdk.New()
	defer wc.Close()
	go wc.Heartbeat(cfgCtx, "shard", weftsdk.DefaultHeartbeatInterval)
	go wc.RuntimeStats(cfgCtx, "shard", weftsdk.DefaultRuntimeStatsInterval)
	go runShardStats(cfgCtx, wc, srv, *dataDir, weftsdk.DefaultRuntimeStatsInterval)

	// HTTP health endpoint.
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]any{"shard_id": srv.Identity()}
		if ds := srv.Shard(); ds != nil {
			resp["row_count"] = ds.RowCount()
		}
		json.NewEncoder(w).Encode(resp)
	})

	httpSrv := &http.Server{Addr: fmt.Sprintf(":%d", *httpPort)}

	// Signal handling for clean shutdown. `go build -cover` only
	// flushes counter files on main() return; SIGKILL skips it.
	// Trigger GracefulStop on gRPC and Shutdown on HTTP so both
	// servers return cleanly, then fall through to main()'s exit.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigs
		log.Printf("shard: received %s, gracefully stopping", s)
		gs.GracefulStop()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	log.Printf("shard HTTP listening on :%d", *httpPort)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Printf("http serve error: %v", err)
		os.Exit(1)
	}
}

// shardIdentityHint peeks at shard.json and returns the shard_id (or ""
// if the file is missing). Done without opening DuckDB so a decision
// about blank vs. reopen can be made first.
func shardIdentityHint(dataDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "shard.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	var m struct {
		ShardID string `json:"shard_id"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", err
	}
	return m.ShardID, nil
}

// runShardStats emits a combined disk-footprint + query-latency sample every
// interval until ctx is cancelled, mirroring the SDK's RuntimeStats cadence.
// Records go through the weft-go SDK (wc.Send) — the same client and transport
// as the Heartbeat/RuntimeStats emitters above; this code never touches the
// records socket itself. The SDK frames each record onto the local weftd
// records socket, weftd relays it over gRPC to the coordinator's ingest, and it
// lands as log rows (namespace=weft, service=shard, kind=stats) — there is no
// stats.log file on the shard. At query time those rows are addressed via the
// pseudofile /log/weft/shard/stats.log, which a cron watch reads and a dashboard
// tile charts. Disk and
// query metrics ride one record on purpose: a dashboard tile reads a single
// source, so carrying volume fill (volume_used_pct, total_bytes) and query
// latency (q_max_ms, q_mean_ms) on the same timestamped row makes the
// correlation we care about — does performance degrade as the disk fills? — a
// trivial two-column read rather than a join across two streams. Disk fields are
// an instantaneous snapshot; query fields aggregate the interval just elapsed.
// The schema lives here, not in the shared weft-go SDK, because it's
// shard-specific.
func runShardStats(ctx context.Context, wc *weftsdk.Client, srv *shardsvc.Server, dataDir string, interval time.Duration) {
	if interval <= 0 {
		interval = weftsdk.DefaultRuntimeStatsInterval
	}
	emit := func() {
		ds := srv.Shard()
		if ds == nil {
			return // blank shard, awaiting Configure — nothing to report yet
		}
		u := ds.DiskUsage()
		q := ds.QueryStatsSnapshot()
		rec := map[string]any{
			"type":     "shard_stats",
			"ts":       time.Now().UTC().Format(time.RFC3339Nano),
			"shard_id": srv.Identity(),
			// disk footprint (instantaneous)
			"duckdb_bytes":    u.DuckDBBytes,
			"wal_bytes":       u.WALBytes,
			"spill_tmp_bytes": u.SpillTmpBytes,
			"total_bytes":     u.TotalBytes(),
			// query latency (aggregate over the interval just elapsed)
			"q_count":   q.Count,
			"q_errors":  q.Errors,
			"q_rows":    q.Rows,
			"q_mean_ms": q.MeanMs,
			"q_max_ms":  q.MaxMs,
		}
		// Volume capacity/headroom: the spill ceiling is 90% of FREE disk, so
		// available space — not file size — is what large queries OOM against.
		if total, avail, err := volumeUsage(dataDir); err == nil {
			rec["volume_total_bytes"] = total
			rec["volume_free_bytes"] = avail
			rec["volume_used_bytes"] = total - avail
			if total > 0 {
				rec["volume_used_pct"] = float64(total-avail) / float64(total) * 100
			}
		}
		_ = wc.Send(ctx, "shard/stats", rec) // best-effort; transport logs its own errors
	}
	emit() // immediate first sample so a dashboard has a value before the first tick
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			emit()
		}
	}
}

// volumeUsage returns the total and available bytes of the filesystem holding
// path, via statfs. Available (not free) blocks — the space usable by the shard
// process, excluding the root reserve — is the honest headroom figure.
func volumeUsage(path string) (total, available uint64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return st.Blocks * bs, st.Bavail * bs, nil
}

// runHeartbeat connects to the coordinator and sends a heartbeat every
// hbInterval. Pulls the shard id on each tick so post-Configure the
// heartbeat stream starts carrying the real identity rather than the
// empty-string unclaimed signal.
func runHeartbeat(coordAddr string, grpcPort int, hbInterval time.Duration, srv *shardsvc.Server) {
	myIP := getPrivateIP()
	myAddr := fmt.Sprintf("%s:%d", myIP, grpcPort)
	myInstanceID := getInstanceID()

	conn, err := grpc.NewClient(coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Printf("heartbeat: connect to coordinator: %v", err)
		return
	}
	client := pb.NewWeftCoordinatorClient(conn)

	// Retry initial heartbeat with backoff so we survive starting
	// simultaneously with the coordinator.
	for i := 0; i < 10; i++ {
		if sendHeartbeat(client, myAddr, myInstanceID, srv) {
			break
		}
		time.Sleep(time.Duration(i+1) * time.Second)
	}

	ticker := time.NewTicker(hbInterval)
	defer ticker.Stop()
	for range ticker.C {
		sendHeartbeat(client, myAddr, myInstanceID, srv)
	}
}

// buildHeartbeatRequest materializes the proto from identity snapshot.
// Pulled out so unit tests can assert every outbound field without a
// live gRPC client. The coordinator's christen path needs InstanceId
// to find the pre-attached EBS volume (tag:shard_id=pending-<id>) —
// an empty InstanceId silently drops that lookup.
func buildHeartbeatRequest(shardID, addr, instanceID, state string, rowCount, diskBytes int64) *pb.ShardHeartbeatRequest {
	return &pb.ShardHeartbeatRequest{
		ShardId:    shardID,
		Addr:       addr,
		InstanceId: instanceID,
		State:      state,
		RowCount:   rowCount,
		DiskBytes:  diskBytes, // .duckdb + spill footprint; control-plane view of volume pressure
		BinarySha:  GitSHA,    // compile-time injected by Packer's -ldflags; empty on local builds
	}
}

func sendHeartbeat(client pb.WeftCoordinatorClient, addr, instanceID string, srv *shardsvc.Server) bool {
	shardID := srv.Identity()
	state := "INGESTING"
	if shardID == "" {
		state = "ADOPTED"
	}

	var rowCount, diskBytes int64
	if ds := srv.Shard(); ds != nil {
		rowCount = ds.RowCount()
		diskBytes = ds.DiskUsage().TotalBytes()
	}

	req := buildHeartbeatRequest(shardID, addr, instanceID, state, rowCount, diskBytes)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Attach the per-shard bearer token so the coordinator's Heartbeat
	// handler can prove this heartbeat came from the real owner of
	// shardID. Empty token on christen-pending shards is expected — the
	// coordinator skips auth for empty-ID heartbeats.
	if tok := srv.HeartbeatToken(); tok != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok)
	}

	if _, err := client.Heartbeat(ctx, req); err != nil {
		log.Printf("heartbeat: %v", err)
		return false
	}
	return true
}

// getInstanceID returns the shard's instance id, the key the coordinator
// correlates a first heartbeat against the LAUNCHING shard SM it minted
// (heartbeatMatchesLaunching). Production reads it from IMDSv2 (the EC2
// instance id). Off-EC2 there's no IMDS, so the dev DockerLauncher injects
// a synthetic id via SHARD_INSTANCE_ID at launch — the same id it stamps
// on the shard SM — and we report that back. Env wins so the synthetic id
// can't be shadowed by a stray IMDS hit. Empty only when neither is set
// (a bare local run), which the coordinator tolerates.
func getInstanceID() string {
	if id := os.Getenv("SHARD_INSTANCE_ID"); id != "" {
		return id
	}
	return imdsFetch("instance-id")
}

// getPrivateIP returns the instance's private IP. Tries EC2 instance
// metadata first (works on EC2), falls back to os.Hostname() for
// docker/dev.
func getPrivateIP() string {
	if ip := imdsFetch("local-ipv4"); ip != "" {
		return ip
	}
	hostname, _ := os.Hostname()
	return hostname
}

// imdsFetch pulls a single IMDSv2 metadata key, e.g. "instance-id" or
// "local-ipv4". Returns "" on any failure (no token, no route, bad
// response) so callers can handle off-EC2 gracefully.
func imdsFetch(key string) string {
	client := &http.Client{Timeout: 2 * time.Second}
	tokenReq, _ := http.NewRequest("PUT", "http://169.254.169.254/latest/api/token", nil)
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokenResp, err := client.Do(tokenReq)
	if err != nil {
		return ""
	}
	defer tokenResp.Body.Close()
	tokenBuf := make([]byte, 256)
	n, _ := tokenResp.Body.Read(tokenBuf)
	if n == 0 {
		return ""
	}
	token := string(tokenBuf[:n])

	req, _ := http.NewRequest("GET", "http://169.254.169.254/latest/meta-data/"+key, nil)
	req.Header.Set("X-aws-ec2-metadata-token", token)
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	buf := make([]byte, 128)
	n, _ = resp.Body.Read(buf)
	if n == 0 {
		return ""
	}
	return string(buf[:n])
}
