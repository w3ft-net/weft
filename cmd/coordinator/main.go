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

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/graybeardcorp/saaskit/email"
	"github.com/graybeardcorp/weft/auth"
	"github.com/graybeardcorp/weft/compute"
	weftcfg "github.com/graybeardcorp/weft/config"
	"github.com/graybeardcorp/weft/coordinator"
	"github.com/graybeardcorp/weft/coordinator/poller"
	coordsvc "github.com/graybeardcorp/weft/coordinator/service"
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
		log.Printf("coordinator: SIGUSR1 goroutine dump failed: %v", err)
		return
	}
	log.Printf("coordinator: SIGUSR1 — wrote %d goroutines (%d bytes) to %s", runtime.NumGoroutine(), len(buf), path)
}

func main() {
	port := flag.Int("port", 9020, "gRPC listen port")
	healthPort := flag.Int("health-port", 9021, "HTTP liveness listen port")
	adminPort := flag.Int("admin-port", 9022, "gRPC listen port for the localhost-only WeftAdmin surface (weft-admin CLI)")
	debugPort := flag.Int("debug-port", 6060, "localhost-only HTTP port for net/http/pprof (goroutine/heap profiling); 0 disables")
	computeAddr := flag.String("compute", "", "compute worker address (host:port)")
	awsRegion := flag.String("region", "us-east-2", "AWS region for shard EC2 launches + volume management")
	customer := flag.String("customer", "", "fallback customer for dev/docker (queries pass customer per-request)")
	dbDSN := flag.String("db-dsn", "", "PostgreSQL DSN (e.g., host=localhost dbname=weft user=weft password=...)")
	queryLogPath := flag.String("query-log", "/var/log/weft/queries.log", "path for the query-audit log; falls back to stderr if unwritable")
	flag.Parse()

	// Route the query-audit stream to its own file so substring monitors
	// watching weft/coordinator can't self-trigger on the audit probe.
	// On failure we keep the stderr fallback rather than fail to start.
	if err := coordinator.SetQueryLog(*queryLogPath); err != nil {
		log.Printf("warning: query-audit log to %s unavailable, using stderr: %v", *queryLogPath, err)
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

	// Compute pool setup.
	var pool compute.ComputePool
	if *computeAddr != "" {
		p, err := compute.NewGRPCComputePool(*computeAddr)
		if err != nil {
			log.Fatalf("connect to compute at %s: %v", *computeAddr, err)
		}
		pool = p
		log.Printf("connected to compute at %s", *computeAddr)
	} else {
		log.Fatal("no compute addr specified")
	}

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
		log.Println("WARNING: using default DB DSN — set --db-dsn or DB_DSN in production")
	}
	d, err := store.Open(dsn)
	if err != nil {
		log.Fatalf("store.Open: %v", err)
	}
	db = d
	defer db.Close()

	// Notifier is built from db so it can pre-resolve aliases for the
	// alert stage. Sharing one instance between Coordinator and
	// CronExecutor below keeps interactive `alert --notify=foo` and
	// scheduled-watch dispatch on the same path.
	notifier := coordinator.NewNotifier(db, email.ConfigFromEnv())

	coord := coordinator.NewCoordinator(coordinator.CoordinatorConfig{
		Pool:              pool,
		Store:             db,
		Notifier:          notifier,
		Tail:              coordinator.NewTailBroker(),
		Ingest:            coordinator.NewRingBuffer(1_000_000),
		Usage:             coordinator.NewCustomerUsageStats(),
		LambdaFunctionURL: os.Getenv("LAMBDA_FUNCTION_URL"),
	})

	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()
	coord.StartDrain(drainCtx)

	// Provisioner manages shard lifecycle — no separate discovery loop.
	// Shards are persisted to postgres and restored on restart via LoadFromStore.
	provCtx, provCancel := context.WithCancel(context.Background())
	defer provCancel()

	// Shard-data control plane: registry hydration, provisioner +
	// launcher, lifecycle poller. All policy from env lives inside
	// logdb (SHARD_<TIER>_SUBNETS / SHARD_<TIER>_LAUNCH_TEMPLATE select
	// EC2-direct launching); only the runtime-derived inputs (db, secret,
	// region) cross the boundary. logdb.Start owns the registry and
	// returns it inside the provisioner.
	prov, err := logdb.Start(logdb.Config{
		Ctx:         provCtx,
		DB:          db,
		ShardSecret: shardSecret,
		AWSRegion:   *awsRegion,
	})
	if err != nil {
		log.Fatalf("logdb.Start: %v", err)
	}
	coord.Provisioner = prov

	// Post-provisioner db-dependent setup: usage metering, customer
	// seeding, archived-shard rehydration, pollers. Everything above
	// already took its snapshot of the store at boot time for
	// hydration; this block covers the pieces that run for the life
	// of the process or depend on live buckets.
	{
		// Start usage metering flush (every 5 minutes).
		coord.StartUsageFlush(drainCtx, db)

		// Seed dev data when running with a customer flag (dev/docker mode).
		if *customer != "" {
			cust, _ := db.FindOrCreateOrg(*customer)
			if cust != nil {
				db.SeedDevSubscription(cust.ID)
				db.SeedDevAgentToken(cust.ID, "dev-agent", *customer)
			}
		}

		// Start CloudWatch poller for configured sources.
		p := poller.New(db, coord)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p.Start(ctx)

		// Cron executor reuses the Notifier already wired into coord.
		cronExec := &coordinator.CronExecutor{
			Store:    db,
			Coord:    coord,
			Notifier: notifier,
		}
		cronExec.Start(ctx)
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
		log.Println("WARNING: using default JWT secret — set JWT_SECRET in production")
	}
	skip := auth.SkipMethods{
		"/weft.WeftCoordinator/IngestStream":    true,
		"/weft.WeftCoordinator/SubmitSample":    true,
		"/weft.WeftCoordinator/GetSourceStatus": true,
		"/weft.WeftCoordinator/Authenticate":    true,
		"/weft.WeftCoordinator/Heartbeat":       true,
	}

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

	// HTTP liveness endpoint. Deliberately minimal — just 200 OK on
	// /health so docker compose's healthcheck, the integration harness
	// (integration/harness.go:217), and any external watchdog can tell
	// the process is up and accepting requests. No subsystem metrics
	// here; if we ever need that, expose it on the gRPC API behind
	// auth, not on an open HTTP port.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, "ok")
		})
		addr := fmt.Sprintf(":%d", *healthPort)
		log.Printf("coordinator health on %s", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Printf("health listener exited: %v", err)
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
			log.Printf("coordinator pprof on %s (localhost only)", addr)
			if err := http.ListenAndServe(addr, nil); err != nil { // nil → DefaultServeMux, where net/http/pprof registered
				log.Printf("pprof listener exited: %v", err)
			}
		}()
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer(
		grpc.MaxRecvMsgSize(bootCfg.GRPCMaxRecvBytes),
		grpc.UnaryInterceptor(auth.UnaryServerInterceptor(
			func(uid, org string) (*auth.Session, error) { return auth.BuildSession(db, uid, org) },
			jwtSecret, skip)),
		grpc.StreamInterceptor(auth.StreamServerInterceptor(
			func(uid, org string) (*auth.Session, error) { return auth.BuildSession(db, uid, org) },
			jwtSecret, skip)),
	)
	coordSvc := &coordsvc.Server{
		Coord:  coord,
		Store:  db,
		CfgPtr: cfgPtr,
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
		log.Printf("coordinator: WeftAdmin gRPC listening on 127.0.0.1:%d (SharedToken auth)", *adminPort)
		go func() {
			if err := adminSrv.Serve(adminLis); err != nil {
				log.Printf("admin serve error: %v", err)
			}
		}()
		defer adminSrv.GracefulStop()
	} else {
		log.Printf("coordinator: WEFT_ADMIN_TOKEN unset — WeftAdmin gRPC listener disabled; weft-admin CLI calls will not be served")
	}

	wc, _ := weftsdk.New()
	defer wc.Close()
	go wc.Heartbeat(cfgCtx, "coordinator", weftsdk.DefaultHeartbeatInterval)
	go wc.RuntimeStats(cfgCtx, "coordinator", weftsdk.DefaultRuntimeStatsInterval)

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
			log.Printf("coordinator: received %s, gracefully stopping", s)
			srv.GracefulStop()
			return
		}
	}()

	if err := srv.Serve(lis); err != nil {
		log.Printf("serve error: %v", err)
		os.Exit(1)
	}
}
