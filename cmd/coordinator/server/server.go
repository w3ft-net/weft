package server

import (
	"context"
	"flag"
	"fmt"
	"github.com/w3ft-net/weft/transport"
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

	"github.com/google/uuid"
	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/w3ft-net/weft/backends/coordination"
	"github.com/w3ft-net/weft/cluster"
	"github.com/w3ft-net/weft/cmd/coordinator/server/options"
	"github.com/w3ft-net/weft/compute"
	weftcfg "github.com/w3ft-net/weft/config"
	"github.com/w3ft-net/weft/coordinator"
	coordsvc "github.com/w3ft-net/weft/coordinator/service"
	"github.com/w3ft-net/weft/deployment"
	"github.com/w3ft-net/weft/grpckeepalive"
	"github.com/w3ft-net/weft/identity"
	"github.com/w3ft-net/weft/ingest"
	"github.com/w3ft-net/weft/logdb"
	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
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

// Flags are the coordinator's command-line flags, parsed, and the log file
// logging was started with.
type Flags struct {
	options.Flags
	logFile *weftsdk.LogFile
}

// ParseFlags starts logging, then defines and parses the coordinator's flags,
// over the cluster file if there is one (--config, or $WEFT_CONFIG). An empty --db-dsn is filled from DB_DSN, so DBDSN is the DSN to open (empty:
// the deployment's own default). Call it before opening anything that logs.
func ParseFlags() *Flags {
	logFile := weftsdk.InitLogging("", os.Stderr)
	logFile.CaptureStdio()
	fl := options.Define(flag.CommandLine)
	cfgPath := cluster.DefineFlag(flag.CommandLine)
	flag.Parse()
	if err := cluster.Overlay(flag.CommandLine, cluster.Coordinator, *cfgPath); err != nil {
		log.Fatalf("coordinator: %v", err)
	}
	if fl.DBDSN == "" {
		fl.DBDSN = os.Getenv("DB_DSN")
	}
	return &Flags{Flags: *fl, logFile: logFile}
}

// Run runs the coordinator process over the deployment deps describes, which
// the caller has opened (and will close when Run returns). Run returns when the
// server stops; a start that cannot proceed exits the process, as
// cmd/coordinator always has.
func Run(flags *Flags, dep *deployment.Deployment, own *Own) {
	logFile := flags.logFile
	fl := &flags.Flags
	if err := fl.TLS.Apply(); err != nil {
		log.Fatalf("coordinator: %v", err)
	}
	if fl.ShareURLBase == "" {
		log.Fatalf("coordinator: --share-url-base is required: the origin clipboard short links are built on")
	}
	if err := validate(dep, own); err != nil {
		log.Fatalf("coordinator: %v", err)
	}
	planner.SetDialects(dep.Dialects)
	port, healthPort, localAddr, internalAddr := &fl.Port, &fl.HealthPort, &fl.LocalAddr, &fl.InternalAddr
	adminPort, debugPort, computeAddr := &fl.AdminPort, &fl.DebugPort, &fl.ComputeAddr
	redisAddr, redisTLS, awsRegion := &fl.RedisAddr, &fl.RedisTLS, &fl.AWSRegion
	shareURLBase, customer, queryLogPath := &fl.ShareURLBase, &fl.Customer, &fl.QueryLogPath

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
	// alert dispatch, and weft.* all have a group-scoped backend from
	// the first request. Without this ordering, per-request customer
	// resolution in query() silently no-ops.
	b := own.Backends
	ext := own.Extension
	if ext == nil {
		ext = NopExtension{}
	}

	// Ack-cycling weftd agents open a stream per active file every commit
	// interval; keep the last-seen stamp off the database for those.
	var agentSeen interface {
		Touch(tokenID uint, clientID string, at time.Time)
	}
	if tracker := b.AgentSeen; tracker != nil {
		agentSeen = tracker
		seenCtx, seenCancel := context.WithCancel(context.Background())
		seenDone := make(chan struct{})
		go func() {
			defer close(seenDone)
			tracker.Run(seenCtx, 30*time.Second)
		}()
		defer func() { seenCancel(); <-seenDone }() // runs before closeDB
	}

	// Notifier is built from db so it can pre-resolve aliases for the
	// alert stage. Sharing one instance between Coordinator and
	// CronExecutor below keeps interactive `alert --notify=foo` and
	// scheduled-watch dispatch on the same path.
	notifier := coordinator.NewNotifier(dep.Backends.Notify, dep.AlertTargets...)

	// Cross-coordinator tail -f: over Redis when an address is given, else
	// local to this process.
	var tailBus coordinator.TailBus
	if *redisAddr != "" {
		tailBus = coordinator.NewRedisTailBus(*redisAddr, *redisTLS)
	}

	coordCfg := CoordinatorConfig(dep, own, coordinator.CoordinatorConfig{
		Compute:           computeClient,
		Notifier:          notifier,
		Tail:              coordinator.NewTailBroker(),
		Ingest:            coordinator.NewRingBuffer(1_000_000),
		DLQ:               b.DeadLetters,
		Usage:             coordinator.NewCustomerUsageStats(),
		LambdaFunctionURL: os.Getenv("LAMBDA_FUNCTION_URL"),
		TailBus:           tailBus,
		ShareURLBase:      *shareURLBase,
	})
	coord := coordinator.NewCoordinator(coordCfg)

	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()
	coord.StartDrain(drainCtx)

	// Provisioner manages shard lifecycle — no separate discovery loop.
	// Shards are persisted to postgres and restored on restart via LoadFromStore.
	provCtx, provCancel := context.WithCancel(context.Background())
	defer provCancel()

	// Identifies this process for the lease table (cron mutual
	// exclusion, and now the shard-fsm-leader lease). Computed before
	// logdb.Start so the provisioner's own election loop can use it too.
	holder := coordinatorHolder()
	coordn := b.Coordination

	// Tag every query-audit line with the same identity, so a query-
	// routing imbalance across coordinator_desired>1 instances is
	// visible from queries.log alone, matching heartbeats/runtime-stats
	// below.
	coordinator.SetQueryLogCoordinatorID(holder)

	// Shard-data control plane: registry hydration, provisioner +
	// launcher, lifecycle poller. Where shards run is the platform's
	// (today read from the environment: SHARD_<TIER>_SUBNETS select
	// EC2-direct launching, else Docker); the runtime-derived inputs (db,
	// secret, holder) are logdb.Config's. logdb.Start owns the registry and
	// returns it inside the provisioner.
	plat, err := logdb.PlatformFromEnv(provCtx, *awsRegion, shardSecret)
	if err != nil {
		log.Fatalf("shard platform: %v", err)
	}
	prov, err := logdb.Start(logdb.Config{
		Ctx:         provCtx,
		Shards:      b.Shards,
		Services:    b.Services,
		Standbys:    b.Standbys,
		ShardSecret: shardSecret,
		Holder:      holder,
		Leases:      coordn,
		Retention:   own.Retention,

		LegacyFactTable: own.LegacyFactTable,
	}, plat)
	if err != nil {
		log.Fatalf("logdb.Start: %v", err)
	}
	coord.Provisioner = prov
	// Stopping the process cancels provCtx, and the election loop releases the
	// shard-fsm-leader lease in its own goroutine. Wait for that release (and
	// for whatever the adapter registered through Runtime.AwaitOnExit) before
	// main returns, or the next coordinator waits out a whole lease TTL as a
	// follower. Deferred after the cancels it makes, so it runs before them.
	exitWaits := []<-chan struct{}{prov.LeaderElectionDone()}
	defer func() {
		provCancel()
		drainCancel()
		deadline := time.After(5 * time.Second)
		for _, done := range exitWaits {
			select {
			case <-done:
			case <-deadline:
				return
			}
		}
	}()
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
	cfgPtr := weftcfg.Watch(cfgCtx, weftcfg.SourceFromEnv(*awsRegion, own.ConfigKinds...), 60*time.Second)
	bootCfg := cfgPtr.Load()

	// Post-provisioner db-dependent setup: usage metering, customer
	// seeding, archived-shard rehydration. Everything above
	// already took its snapshot of the store at boot time for
	// hydration; this block covers the pieces that run for the life
	// of the process or depend on live buckets.
	{
		// The deployment's own startup work that needs the store and the
		// running coordinator (hosted: usage metering, dev seeding).
		ext.Start(Runtime{
			Coord: coord, Ctx: drainCtx, Customer: *customer,
			Holder: holder, Leases: coordn, AWSRegion: *awsRegion, InstanceID: imdsInstanceID(),
			AwaitOnExit: func(done <-chan struct{}) { exitWaits = append(exitWaits, done) },
		})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Cron executor reuses the Notifier already wired into coord.
		cronExec := &coordinator.CronExecutor{
			Coord:    coord,
			Notifier: notifier,
			Holder:   holder,
			Claims:   coordn,
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
	ids := dep.Authn.GRPC
	skip := coordsvc.PublicMethods

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
	// Three listeners over one service: the public port serves only
	// service.PublicMethods (ingest); the internal port serves only InternalMethods to
	// shards inside the VPC; the loopback listener serves everything to the
	// co-located api.
	newGRPC := func(allowed, noJWT identity.SkipMethods) *grpc.Server {
		unary := []grpc.UnaryServerInterceptor{weftsdk.GRPC.UnaryServerInterceptor()}
		stream := []grpc.StreamServerInterceptor{weftsdk.GRPC.StreamServerInterceptor()}
		if allowed != nil {
			unary = append(unary, coordsvc.OnlyUnaryInterceptor(allowed))
			stream = append(stream, coordsvc.OnlyStreamInterceptor(allowed))
		}
		authUnary, authStream := ids.Interceptors(noJWT)
		return grpc.NewServer(append(transport.ServerOptions(),
			grpc.MaxRecvMsgSize(bootCfg.GRPCMaxRecvBytes),
			// Accept the keepalive pings weftd sends to notice a dead
			// connection; the default refuses anything faster than every five
			// minutes with GOAWAY too_many_pings.
			grpc.KeepaliveEnforcementPolicy(grpckeepalive.ServerEnforcement),
			grpc.ChainUnaryInterceptor(append(unary, authUnary...)...),
			grpc.ChainStreamInterceptor(append(stream, authStream...)...),
		)...)
	}
	srv := newGRPC(coordsvc.PublicMethods, skip)
	localSrv := newGRPC(nil, skip)
	internalSrv := newGRPC(coordsvc.InternalMethods, coordsvc.InternalMethods)
	localLis, err := net.Listen("tcp", *localAddr)
	if err != nil {
		log.Fatalf("local listen on %s: %v", *localAddr, err)
	}
	internalLis, err := net.Listen("tcp", *internalAddr)
	if err != nil {
		log.Fatalf("internal listen on %s: %v", *internalAddr, err)
	}
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
		Tokens:     dep.Backends.Tokens,
		Sources:    dep.Backends.Sources,
		Authz:      coordCfg.Authz,
		CfgPtr:     cfgPtr,
		Classifier: classifier,
		AgentSeen:  agentSeen,

		ShardSecret: shardSecret,
	}
	ext.ConfigureService(coordSvc)
	pb.RegisterWeftCoordinatorServer(srv, coordSvc)
	pb.RegisterWeftCoordinatorServer(localSrv, coordSvc)
	pb.RegisterWeftCoordinatorServer(internalSrv, coordSvc)
	go func() {
		if err := internalSrv.Serve(internalLis); err != nil {
			weftsdk.Errorf(context.Background(), "internal serve error: %v", err)
		}
	}()
	defer internalSrv.GracefulStop()
	go func() {
		if err := localSrv.Serve(localLis); err != nil {
			weftsdk.Errorf(context.Background(), "local serve error: %v", err)
		}
	}()
	defer localSrv.GracefulStop()

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
	// data into one stream.
	telemetryFields := map[string]any{"coordinator_id": holder}
	go wc.HeartbeatWithFields(cfgCtx, "coordinator", weftsdk.DefaultHeartbeatInterval, telemetryFields)
	go wc.RuntimeStatsWithFields(cfgCtx, "coordinator", weftsdk.DefaultRuntimeStatsInterval, telemetryFields)
	// Every minute, so a cron on any schedule sees who holds each lease.
	go coordination.LogLeaseStates(cfgCtx, coordn, holder, time.Minute)

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
			internalSrv.GracefulStop()
			localSrv.GracefulStop()
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
