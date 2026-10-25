package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on DefaultServeMux; served on the localhost-only debug listener below
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"time"

	"google.golang.org/grpc"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/google/uuid"
	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/saaskit/email"
	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/compute"
	weftcfg "github.com/graybeardcorp/weft/config"
	"github.com/graybeardcorp/weft/coordinator"
	coordsvc "github.com/graybeardcorp/weft/coordinator/service"
	"github.com/graybeardcorp/weft/ingest"
	"github.com/graybeardcorp/weft/logdb"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/store"
)

// dumpGoroutines writes a full all-goroutine stack trace to
// /tmp/weft-coordinator-goroutines.txt on receipt of SIGUSR1. Used to
// diagnose goroutine leaks in prod without bouncing the process: an
// operator `kevcons ssh`es in, `kill -USR1`s the coordinator, and cats
// the file. The buffer is grown until the whole dump fits (runtime.Stack
// truncates rather than reallocating).
func dumpGoroutines() {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	const path = "/tmp/weft-coordinator-goroutines.txt"
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		weftsdk.Errorf(context.Background(), "coordinator: SIGUSR1 goroutine dump failed: %v", err)
		return
	}
	weftsdk.Infof(context.Background(), "coordinator: SIGUSR1 — wrote %d goroutines (%d bytes) to %s", runtime.NumGoroutine(), len(buf), path)
}

// coordinatorHolder identifies this process for lease ownership (see
// store.AcquireLease) — it only needs to be unique per running
// coordinator, not stable across restarts. Prefers the EC2 instance-id
// (recognizable in `SELECT * FROM leases` without a lookup); falls back
// to hostname+random for local/docker dev, where there's no IMDS.
func coordinatorHolder() string {
	if id := imdsInstanceID(); id != "" {
		return id
	}
	hostname, _ := os.Hostname()
	return fmt.Sprintf("%s-%s", hostname, uuid.NewString()[:8])
}

// imdsInstanceID fetches the EC2 instance-id via IMDSv2. Returns "" on
// any failure (no token, no route, bad response) so callers can handle
// off-EC2 gracefully — mirrors cmd/shard/main.go's imdsFetch.
func imdsInstanceID() string {
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

	req, _ := http.NewRequest("GET", "http://169.254.169.254/latest/meta-data/instance-id", nil)
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

func main() {
	logFile := weftsdk.InitLogging("", os.Stderr)
	logFile.CaptureStdio()
	port := flag.Int("port", 9020, "gRPC listen port")
	healthPort := flag.Int("health-port", 9021, "HTTP liveness listen port")
	adminPort := flag.Int("admin-port", 9022, "gRPC listen port for the localhost-only WeftAdmin surface (weft-admin CLI)")
	debugPort := flag.Int("debug-port", 6060, "localhost-only HTTP port for net/http/pprof (goroutine/heap profiling); 0 disables")
	computeAddr := flag.String("compute", "", "compute worker address (host:port)")
	redisAddr := flag.String("redis-addr", "", "Redis address for cross-coordinator tail -f fan-out (host:port); empty disables cross-coordinator fan-out, tail stays local to this process")
	redisTLS := flag.Bool("redis-tls", false, "use TLS for the Redis connection (required for ElastiCache in-transit encryption)")
	awsRegion := flag.String("region", "us-east-2", "AWS region for shard EC2 launches + volume management")
	shareURLBase := flag.String("share-url-base", "https://w3ft.net", "origin `clipboard` short links are built on")
	customer := flag.String("customer", "", "fallback customer for dev/docker (queries pass customer per-request)")
	dbDSN := flag.String("db-dsn", "", "PostgreSQL DSN (e.g., host=localhost dbname=weft user=weft password=...)")
	queryLogPath := flag.String("query-log", "/var/log/weft/queries.log", "path for the query-audit log; falls back to stderr if unwritable")
	flag.Parse()

	// Route the query-audit stream to its own file so substring monitors
	// watching weft/coordinator can't self-trigger on the audit probe.
	// On failure we keep the stderr fallback rather than fail to start.
	// With SDK logging the audit stream rides the main log file; the file flag
	// only serves runs without WEFT_LOG_FILE.
	coordinator.SetQueryLogFile(logFile)
	if logFile == nil {
		if err := coordinator.SetQueryLog(*queryLogPath); err != nil {
			weftsdk.Warnf(context.Background(), "warning: query-audit log to %s unavailable, using stderr: %v", *queryLogPath, err)
		}
	}

	// Shared bearer token for coord ↔ shard gRPC. The query path no
	// longer needs this — compute reads SHARD_SECRET from its own env
	// and dials shards itself. We still read it here because the
	// coordinator dials shards directly for Ingest (drain loop),
	// Archive (rotation), boot rehydration (ReconstructShard), and the
	// dev DockerLauncher (which injects it into spawned shard
	// containers as the server-side credential).
	shardSecret := os.Getenv("SHARD_SECRET")
	if shardSecret == "" {
		log.Fatal("no SHARD_SECRET set")
	}

	if *computeAddr == "" {
		log.Fatal("no compute addr specified")
	}
	computeClient, err := compute.NewClient(*computeAddr)
	if err != nil {
		log.Fatalf("connect to compute at %s: %v", *computeAddr, err)
	}
	weftsdk.Infof(context.Background(), "connected to compute at %s", *computeAddr)

	// Open the store before building the Coordinator so /bin lookup,
	// alert dispatch, and weft.* all have a tenant-scoped backend from
	// the first request. Without this ordering, per-request customer
	// resolution in query() silently no-ops.
	var db *store.Store
	dsn := *dbDSN
	if dsn == "" {
		dsn = os.Getenv("DB_DSN")
	}
	if dsn == "" {
		dsn = "host=localhost dbname=weft user=weft password=weft sslmode=disable"
		weftsdk.Warnf(context.Background(), "using default DB DSN — set --db-dsn or DB_DSN in production")
	}
	if err := store.Migrate(store.MigrateDSN(dsn)); err != nil {
		log.Fatalf("store.Migrate: %v", err)
	}
	d, err := store.Connect(dsn)
	if err != nil {
		log.Fatalf("store.Connect: %v", err)
	}
	db = d
	defer db.Close()

	// Ack-cycling weftd agents open a stream per active file every commit
	// interval; keep auth and the last-seen stamp off the database for those.
	db.EnableAuthCache()
	agentSeen := store.NewAgentSeenTracker(db)
	seenCtx, seenCancel := context.WithCancel(context.Background())
	seenDone := make(chan struct{})
	go func() {
		defer close(seenDone)
		agentSeen.Run(seenCtx, 30*time.Second)
	}()
	defer func() { seenCancel(); <-seenDone }() // runs before db.Close

	// Notifier is built from db so it can pre-resolve aliases for the
	// alert stage. Sharing one instance between Coordinator and
	// CronExecutor below keeps interactive `alert --notify=foo` and
	// scheduled-watch dispatch on the same path.
	notifier := coordinator.NewNotifier(db, email.ConfigFromEnv())

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{
		Compute:           computeClient,
		Store:             db,
		Notifier:          notifier,
		Tail:              coordinator.NewTailBroker(),
		Ingest:            coordinator.NewRingBuffer(1_000_000),
		DLQ:               coordinator.NewDLQ(*awsRegion, os.Getenv("DLQ_BUCKET")),
		Usage:             coordinator.NewCustomerUsageStats(),
		LambdaFunctionURL: os.Getenv("LAMBDA_FUNCTION_URL"),
		RedisAddr:         *redisAddr,
		RedisTLS:          *redisTLS,
		ShareURLBase:      *shareURLBase,
	})

	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()
	coord.StartDrain(drainCtx)

	// Provisioner manages shard lifecycle — no separate discovery loop.
	// Shards are persisted to postgres and restored on restart via LoadFromStore.
	provCtx, provCancel := context.WithCancel(context.Background())
	defer provCancel()

	// Identifies this process for the lease table (cron/NAT mutual
	// exclusion, and now the shard-fsm-leader lease — see
	// docs/plans/coordinator-horizontal-scaling.md #5). Computed before
	// logdb.Start so the provisioner's own election loop can use it too.
	holder := coordinatorHolder()

	// Tag every query-audit line with the same identity, so a query-
	// routing imbalance across coordinator_desired>1 instances is
	// visible from queries.log alone, matching heartbeats/runtime-stats
	// below. See docs/known-issues.md, "queries.log isn't tagged with
	// coordinator_id".
	coordinator.SetQueryLogCoordinatorID(holder)

	// Shard-data control plane: registry hydration, provisioner +
	// launcher, lifecycle poller. All policy from env lives inside
	// logdb (SHARD_<TIER>_SUBNETS / SHARD_<TIER>_LAUNCH_TEMPLATE select
	// EC2-direct launching); only the runtime-derived inputs (db, secret,
	// region, holder) cross the boundary. logdb.Start owns the registry
	// and returns it inside the provisioner.
	prov, err := logdb.Start(logdb.Config{
		Ctx:         provCtx,
		DB:          db,
		ShardSecret: shardSecret,
		AWSRegion:   *awsRegion,
		Holder:      holder,
	})
	if err != nil {
		log.Fatalf("logdb.Start: %v", err)
	}
	coord.Provisioner = prov
	coord.StartDLQReplay(drainCtx, time.Minute, prov.IsLeader)

	// Runtime config watcher — powers IngestStream's per-stream byte
	// budget via an atomic pointer the service reads on every accepted
	// stream. GRPCMaxRecvBytes is read exactly once here because gRPC's
	// MaxRecvMsgSize is a ServerOption evaluated at NewServer time.
	appName := os.Getenv("APP_NAME")
	if appName == "" {
		appName = "weft"
	}
	cfgCtx, cfgCancel := context.WithCancel(context.Background())
	defer cfgCancel()
	cfgPtr := weftcfg.Watch(cfgCtx, appName, *awsRegion, 60*time.Second)
	bootCfg := cfgPtr.Load()

	// Post-provisioner db-dependent setup: usage metering, customer
	// seeding, archived-shard rehydration. Everything above
	// already took its snapshot of the store at boot time for
	// hydration; this block covers the pieces that run for the life
	// of the process or depend on live buckets.
	{
		// Start usage metering flush (every 30 seconds).
		coord.StartUsageFlush(drainCtx, db)

		// Seed dev data when running with a customer flag (dev/docker mode).
		if *customer != "" {
			cust, _ := db.FindOrCreateOrg(*customer)
			if cust != nil {
				db.SeedDevSubscription(cust.ID)
				db.SeedDevAgentToken(cust.ID, "dev-agent", *customer)
			}
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Cron executor reuses the Notifier already wired into coord.
		cronExec := &coordinator.CronExecutor{
			Store:    db,
			Coord:    coord,
			Notifier: notifier,
			Holder:   holder,
		}
		cronExec.Start(ctx)

		// NAT self-election: exactly one coordinator instance points the
		// private subnet's 0.0.0.0/0 route at itself, via the same lease
		// primitive as cron above. Replaces the old boot-time-only
		// claim in infrastructure-catalog's shared user-data script,
		// which raced on boot order — see
		// docs/plans/coordinator-horizontal-scaling.md #1. No-ops
		// (leaving NAT to that removed boot claim's absence, i.e. doing
		// nothing) off-EC2 or when NAT_ROUTE_TABLE_ID isn't set — the
		// expected shape in dev/docker.
		if instanceID := imdsInstanceID(); instanceID != "" {
			if routeTableID := os.Getenv("NAT_ROUTE_TABLE_ID"); routeTableID != "" {
				awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(*awsRegion))
				if err != nil {
					weftsdk.Errorf(ctx, "nat: aws config load failed, not running NAT election: %v", err)
				} else {
					ec2Client := ec2.NewFromConfig(awsCfg)
					eniID, err := coordinator.ResolveOwnENI(context.Background(), ec2Client, instanceID)
					if err != nil {
						weftsdk.Errorf(ctx, "nat: resolve own ENI failed, not running NAT election: %v", err)
					} else {
						elector := coordinator.NewNATElector(db, ec2Client, routeTableID, eniID, holder)
						elector.Start(ctx)
					}
				}
			}
		}
	}

	// gRPC server with auth interceptors.
	//
	// JWT-gated RPCs carry the caller's bearer token in metadata; the
	// auth interceptor validates and stashes claims in ctx. A few RPCs
	// skip the JWT check because they bootstrap some other auth model:
	//   - IngestStream / SubmitSample / GetSourceStatus: agent-token
	//     bearer, validated inside the handler.
	//   - Authenticate: takes the agent token in the request body; this
	//     is how weftd converts a wft_... into a customer/namespace.
	//   - Heartbeat: unauthenticated today; separate follow-up will
	//     require per-shard credentials.
	jwtSecret := os.Getenv("JWT_SECRET")
	if jwtSecret == "" {
		jwtSecret = "dev-secret-change-in-production"
		weftsdk.Warnf(context.Background(), "using default JWT secret — set JWT_SECRET in production")
	}
	skip := auth.SkipMethods{
		"/weft.WeftCoordinator/IngestStream":    true,
		"/weft.WeftCoordinator/SubmitSample":    true,
		"/weft.WeftCoordinator/GetSourceStatus": true,
		"/weft.WeftCoordinator/Authenticate":    true,
		"/weft.WeftCoordinator/Heartbeat":       true,
	}

	// HTTP readiness endpoint: 503 until the gRPC server is about to serve
	// (see coordinator.Readiness), then 200 for good. The ASG's load balancer
	// health check, docker compose's healthcheck and the integration harness
	// (integration/harness.go:217) all wait on it. No subsystem metrics
	// here; if we ever need that, expose it on the gRPC API behind
	// auth, not on an open HTTP port.
	ready := &coordinator.Readiness{}
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/health", ready.Handler())
		addr := fmt.Sprintf(":%d", *healthPort)
		weftsdk.Infof(context.Background(), "coordinator health on %s", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			weftsdk.Infof(context.Background(), "health listener exited: %v", err)
		}
	}()

	// Localhost-only pprof. Bound to 127.0.0.1 so the profiling surface
	// is reachable only over an SSM session (kevcons ssh), never from the
	// public network — same trust model as the WeftAdmin listener below.
	// `/debug/pprof/goroutine?debug=1` gives the aggregated stack→count
	// view that makes a goroutine leak obvious (the leaking call site is
	// whichever stack carries the outlier count).
	if *debugPort != 0 {
		go func() {
			addr := fmt.Sprintf("127.0.0.1:%d", *debugPort)
			weftsdk.Infof(context.Background(), "coordinator pprof on %s (localhost only)", addr)
			if err := http.ListenAndServe(addr, nil); err != nil { // nil → DefaultServeMux, where net/http/pprof registered
				weftsdk.Infof(context.Background(), "pprof listener exited: %v", err)
			}
		}()
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(bootCfg.GRPCMaxRecvBytes),
		grpc.ChainUnaryInterceptor(weftsdk.GRPC.UnaryServerInterceptor(), auth.UnaryServerInterceptor(
			func(uid, org string) (*auth.Session, error) { return auth.BuildSession(db, uid, org) },
			jwtSecret, skip)),
		grpc.ChainStreamInterceptor(weftsdk.GRPC.StreamServerInterceptor(), auth.StreamServerInterceptor(
			func(uid, org string) (*auth.Session, error) { return auth.BuildSession(db, uid, org) },
			jwtSecret, skip)),
	)
	// LLM-based schema-field classification for pending sources — see
	// ingest/llmdetect.go. Disabled (nil) unless
	// BEDROCK_CLASSIFIER_MODEL_ID/BEDROCK_REGION are set; deliberately
	// opt-in since it depends on IAM/model-access provisioned separately
	// (infrastructure-live), not something the coordinator should refuse
	// to start over.
	classifier, err := ingest.NewBedrockClassifierFromEnv(context.Background())
	if err != nil {
		weftsdk.Warnf(context.Background(), "WARNING: bedrock classifier disabled: %v", err)
	}

	coordSvc := &coordsvc.Server{
		Coord:      coord,
		Store:      db,
		CfgPtr:     cfgPtr,
		Classifier: classifier,
		AgentSeen:  agentSeen,
	}
	pb.RegisterWeftCoordinatorServer(srv, coordSvc)

	// WeftAdmin lives on a separate 127.0.0.1:<admin-port> listener gated
	// by SharedToken auth (WEFT_ADMIN_TOKEN). It exists so the coordinator-
	// resident `weft-admin` CLI can drive operator/deploy actions
	// (RelaunchShard, etc.) without a JWT, while making those methods
	// unreachable from the public gRPC port — JWT-holders of
	// WeftCoordinator can't call them and a weftd-style bearer can't forge
	// the admin token. Skipped when WEFT_ADMIN_TOKEN is unset: the
	// SharedToken interceptor refuses to start with an empty expected token
	// (no silent any-caller mode), so admin RPCs are simply unavailable
	// until the operator wires the env. The external 9020 listener is
	// unaffected either way.
	if adminToken := os.Getenv("WEFT_ADMIN_TOKEN"); adminToken != "" {
		adminLis, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *adminPort))
		if err != nil {
			log.Fatalf("admin listen on 127.0.0.1:%d: %v", *adminPort, err)
		}
		adminSrv := coordsvc.NewAdminGRPCServer(coordSvc, adminToken)
		weftsdk.Infof(context.Background(), "coordinator: WeftAdmin gRPC listening on 127.0.0.1:%d (SharedToken auth)", *adminPort)
		go func() {
			if err := adminSrv.Serve(adminLis); err != nil {
				weftsdk.Errorf(context.Background(), "admin serve error: %v", err)
			}
		}()
		defer adminSrv.GracefulStop()
	} else {
		weftsdk.Infof(context.Background(), "coordinator: WEFT_ADMIN_TOKEN unset — WeftAdmin gRPC listener disabled; weft-admin CLI calls will not be served")
	}

	wc, _ := weftsdk.New()
	defer wc.Close()
	// coordinator_id lets dashboards/crons group heartbeats and runtime
	// stats per coordinator instance instead of blending every process's
	// data into one stream — see docs/plans/coordinator-horizontal-scaling.md
	// and the live-testing follow-up plan.
	telemetryFields := map[string]any{"coordinator_id": holder}
	go wc.HeartbeatWithFields(cfgCtx, "coordinator", weftsdk.DefaultHeartbeatInterval, telemetryFields)
	go wc.RuntimeStatsWithFields(cfgCtx, "coordinator", weftsdk.DefaultRuntimeStatsInterval, telemetryFields)
	// Every minute, so a cron on any schedule sees who holds each lease.
	go db.LogLeaseStates(cfgCtx, holder, time.Minute)

	// Signal handling. Needed so `go build -cover` binaries flush
	// coverage counters on `docker compose down` — SIGKILL skips the
	// flush; a SIGTERM→GracefulStop→main-return sequence does not.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	go func() {
		for s := range sigs {
			if s == syscall.SIGUSR1 {
				// Non-destructive full goroutine dump to a file (the
				// process keeps running). Grows the buffer until the whole
				// dump fits, then writes it where `kevcons ssh` can cat it.
				dumpGoroutines()
				continue
			}
			weftsdk.Infof(context.Background(), "coordinator: received %s, gracefully stopping", s)
			srv.GracefulStop()
			return
		}
	}()

	ready.MarkReady()
	if err := srv.Serve(lis); err != nil {
		weftsdk.Errorf(context.Background(), "serve error: %v", err)
		os.Exit(1)
	}
}
