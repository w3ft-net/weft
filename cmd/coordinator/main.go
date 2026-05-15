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
	"runtime"
	"strings"
	"syscall"

	"time"

	"google.golang.org/grpc"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/kevlarcon/weft/auth"
	"github.com/kevlarcon/weft/compute"
	weftcfg "github.com/kevlarcon/weft/config"
	"github.com/kevlarcon/weft/coordinator"
	"github.com/kevlarcon/weft/internal/email"
	"github.com/kevlarcon/weft/coordinator/discovery"
	"github.com/kevlarcon/weft/coordinator/poller"
	coordsvc "github.com/kevlarcon/weft/coordinator/service"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	"github.com/kevlarcon/weft/store"
)

func main() {
	port := flag.Int("port", 9020, "gRPC listen port")
	healthPort := flag.Int("health-port", 9021, "HTTP health listen port")
	shardsFlag := flag.String("shards", "", "comma-separated shard specs: id=addr=start=end,... (for Docker/dev)")
	shardASGSmall := flag.String("shard-asg-small", "", "small tier shard ASG name")
	shardASGMedium := flag.String("shard-asg-medium", "", "medium tier shard ASG name")
	shardASGLarge := flag.String("shard-asg-large", "", "large tier shard ASG name")
	computeAddr := flag.String("compute", "", "compute worker address (host:port)")
	computeASG := flag.String("compute-asg", "", "compute ASG name for AWS discovery (production)")
	awsRegion := flag.String("region", "us-east-2", "AWS region for ASG discovery")
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

	// Shared bearer token for coord ↔ shard gRPC. Coord is the client,
	// shard is the server; both read the same SHARD_SECRET env var and
	// exchange it on every call via auth.ClientToken / SharedToken
	// interceptors. Default matches cmd/shard/main.go so the local
	// docker stack works without additional config.
	shardSecret := os.Getenv("SHARD_SECRET")
	if shardSecret == "" {
		shardSecret = "dev-shard-secret-change-in-production"
		log.Println("WARNING: using default SHARD_SECRET — set SHARD_SECRET in production")
	}

	var shards []shard.Shard

	// Shard setup: explicit specs, legacy single ASG, or tiered ASGs.
	if *shardsFlag != "" {
		shards = parseShardsFlag(*shardsFlag, shardSecret)
	}
	// Provisioner manages shard lifecycle. No startup discovery needed —
	// shards are restored from postgres or bootstrapped by the provisioner.

	// Compute pool setup.
	var pool compute.ComputePool
	if *computeAddr != "" {
		p, err := compute.NewGRPCComputePool(*computeAddr)
		if err != nil {
			log.Fatalf("connect to compute at %s: %v", *computeAddr, err)
		}
		pool = p
		log.Printf("connected to compute at %s", *computeAddr)
	} else if *computeASG != "" {
		disc, err := discovery.NewASGDiscovery(*awsRegion, *computeASG, 9010)
		if err != nil {
			log.Fatalf("init compute discovery: %v", err)
		}
		addrs, err := disc.DiscoverAddrs(context.Background())
		if err != nil || len(addrs) == 0 {
			log.Printf("no compute workers found, using local pool")
			pool = &compute.LocalComputePool{}
		} else {
			p, err := compute.NewGRPCComputePool(addrs[0])
			if err != nil {
				log.Fatalf("connect to compute: %v", err)
			}
			pool = p
			log.Printf("connected to compute at %s (from ASG)", addrs[0])
		}
	} else {
		pool = &compute.LocalComputePool{}
		log.Printf("using local compute pool")
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
		Shards:            shards,
		Pool:              pool,
		Store:             db,
		Notifier:          notifier,
		Tail:              coordinator.NewTailBroker(),
		Ingest:            coordinator.NewRingBuffer(1_000_000),
		Usage:             coordinator.NewCustomerUsageStats(),
		ShardSecret:       shardSecret,
		LambdaFunctionURL: os.Getenv("LAMBDA_FUNCTION_URL"),
	})

	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()
	coord.StartDrain(drainCtx)

	shardTiers := make(map[string]string)
	if *shardASGSmall != "" {
		shardTiers["small"] = *shardASGSmall
	}
	if *shardASGMedium != "" {
		shardTiers["medium"] = *shardASGMedium
	}
	if *shardASGLarge != "" {
		shardTiers["large"] = *shardASGLarge
	}

	// Provisioner manages shard lifecycle — no separate discovery loop.
	// Shards are persisted to postgres and restored on restart via LoadFromStore.
	provCtx, provCancel := context.WithCancel(context.Background())
	defer provCancel()

	// Load AWS credentials once for Lambda SigV4 signing. Used by
	// both the provisioner (archive path) and reconstruct (boot).
	awsCfg, awsCfgErr := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(*awsRegion))
	if awsCfgErr != nil {
		log.Printf("warning: AWS config unavailable — Lambda cold queries will fail: %v", awsCfgErr)
	}
	if awsCfgErr == nil {
		if lp, ok := pool.(*compute.LocalComputePool); ok {
			lp.AWSCreds = awsCfg.Credentials
			lp.AWSRegion = *awsRegion
		}
	}

	// Hydrate the shard registry from postgres BEFORE starting the
	// provisioner or lifecycle poller. The lifecycle handler's
	// recognisedAsDead checks the registry to decide whether an
	// orphan EBS volume should be recovered; an empty registry
	// causes every volume to be abandoned, orphaning data on every
	// deploy. The store-fallback in recognisedAsDead is defense in
	// depth; populating the registry first is the primary fix.
	{
		coord.Registry.SetStore(db)
		s3cfg := shard.S3ConfigFromEnv()
		lambdaURL := os.Getenv("LAMBDA_FUNCTION_URL")
		reconstructCfg := coordinator.ReconstructConfig{
			ShardSecret:       shardSecret,
			LambdaFunctionURL: lambdaURL,
			S3Config:          s3cfg,
		}
		if awsCfgErr == nil {
			reconstructCfg.AWSCredentials = awsCfg.Credentials
			reconstructCfg.AWSRegion = *awsRegion
		}
		if err := coord.Registry.LoadFromStore(func(rec store.ShardRecord) (shard.Shard, error) {
			return coordinator.ReconstructShard(rec, reconstructCfg)
		}); err != nil {
			log.Printf("warning: load shards from store: %v", err)
		} else {
			log.Printf("coordinator: restored shards from store")
		}
	}

	if len(shardTiers) > 0 {
		// Production: ASG launcher for shard EC2 instances.
		asgLauncher, err := coordinator.NewASGLauncher(*awsRegion, shardTiers, 9000)
		if err != nil {
			log.Printf("warning: ASG launcher unavailable: %v", err)
		} else {
			provConfig := coordinator.DefaultProvisionerConfig()
			provConfig.ShardSecret = shardSecret
			provConfig.S3Config = shard.S3ConfigFromEnv()
			provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
			provConfig.LambdaFunctionURL = os.Getenv("LAMBDA_FUNCTION_URL")
			if awsCfgErr == nil {
				provConfig.AWSCredentials = awsCfg.Credentials
				provConfig.AWSRegion = *awsRegion
			}
			prov := coordinator.NewProvisioner(coord.Registry, asgLauncher, provConfig)
			coord.Provisioner = prov

			// Attach a VolumeManager so christen() can tag EBS volumes
			// with the minted shard id. Runs in prod mode; unavailable
			// when the SDK can't load (caller warned above).
			if vm, err := coordinator.NewEC2VolumeManager(*awsRegion); err != nil {
				log.Printf("warning: VolumeManager unavailable: %v", err)
			} else {
				prov.SetVolumeManager(vm)
			}

			prov.SetStore(db)
			// No-cache: SetStore on the bucket manager is all that's
			// needed; every lookup hits the DB.
			prov.Buckets.SetStore(db)

			prov.Start(provCtx)
			log.Printf("provisioner: started with ASG tiers %v", shardTiers)

			// Lifecycle poller: consume ASG launch hook events from SQS
			// and decide christen/recover for each new shard instance.
			// Controlled by LIFECYCLE_QUEUE_URL so dev/docker
			// coordinators stay out of this path.
			if queueURL := os.Getenv("LIFECYCLE_QUEUE_URL"); queueURL != "" {
				tierByASG := make(map[string]string, len(shardTiers))
				for tier, asgName := range shardTiers {
					tierByASG[asgName] = tier
				}
				sizeByTier := readTierSizes()
				lcCfg := coordinator.LifecycleHandlerConfig{
					Registry:   coord.Registry,
					TierByASG:  tierByASG,
					SizeByTier: sizeByTier,
					DeviceName: os.Getenv("SHARD_VOLUME_DEVICE"),
					Store:      db,
					ShardOps:   db,
				}
				poller, err := coordinator.BuildLifecyclePoller(provCtx, *awsRegion, queueURL, lcCfg)
				if err != nil {
					log.Printf("warning: lifecycle poller unavailable: %v", err)
				} else if poller != nil {
					poller.Start(provCtx)
					log.Printf("lifecycle: subscribed to %s", queueURL)
				}
			}
		}
	} else {
		// Dev/Docker: spawn sibling shard containers via DockerLauncher
		// so each shard has a persistent dataDir on a host bind-mount,
		// heartbeats independently, and survives a coordinator process
		// restart. Misconfiguration (missing host data root, missing
		// docker socket) is fatal in this branch — we don't silently
		// fall back to in-process shards anymore. Unit tests use the
		// in-package localLauncher directly, not this code path.
		launcher := buildDockerLauncher(shardSecret)
		provConfig := coordinator.DevProvisionerConfig()
		provConfig.ShardSecret = shardSecret
		provConfig.ParquetDir = os.Getenv("PARQUET_DIR")
		prov := coordinator.NewProvisioner(coord.Registry, launcher, provConfig)
		coord.Provisioner = prov

		prov.SetStore(db)
		prov.Buckets.SetStore(db)

		prov.Start(provCtx)
		log.Printf("provisioner: started with docker launcher (dev mode), image=%s data=%s",
			launcher.Image, launcher.HostDataRoot)
	}

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
			cust, _ := db.FindOrCreateCustomer(*customer)
			if cust != nil {
				db.SeedDevSubscription(cust.Name)
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

	// HTTP health endpoint with subsystem metrics for load testing.
	go func() {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			var mem runtime.MemStats
			runtime.ReadMemStats(&mem)

			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"shards": len(coord.GetShards()),
				"system": map[string]any{
					"goroutines":  runtime.NumGoroutine(),
					"heap_mb":     mem.HeapAlloc / 1024 / 1024,
					"heap_sys_mb": mem.HeapSys / 1024 / 1024,
					"gc_cycles":   mem.NumGC,
				},
			})
		})
		log.Printf("coordinator health on :%d", *healthPort)
		http.ListenAndServe(fmt.Sprintf(":%d", *healthPort), nil)
	}()

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
	pb.RegisterWeftCoordinatorServer(srv, &coordsvc.Server{
		Coord:  coord,
		Store:  db,
		CfgPtr: cfgPtr,
	})

	log.Printf("coordinator listening on :%d (%d shards, customer=%s)", *port, len(shards), *customer)

	wc, _ := weftsdk.New()
	defer wc.Close()
	go wc.Heartbeat(cfgCtx, "coordinator", weftsdk.DefaultHeartbeatInterval)
	go wc.RuntimeStats(cfgCtx, "coordinator", weftsdk.DefaultRuntimeStatsInterval)

	// Signal handling. Needed so `go build -cover` binaries flush
	// coverage counters on `docker compose down` — SIGKILL skips the
	// flush; a SIGTERM→GracefulStop→main-return sequence does not.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigs
		log.Printf("coordinator: received %s, gracefully stopping", s)
		srv.GracefulStop()
	}()

	if err := srv.Serve(lis); err != nil {
		log.Printf("serve error: %v", err)
		os.Exit(1)
	}
}

// readTierSizes returns per-tier data volume sizes from env vars populated
// by load-app-env out of the TF-managed config blob (see
// stacks/asg-weft-application/main.tf → aws_ssm_parameter.app_config_blob).
// Falls back to sensible defaults when a var is missing or unparseable.
func readTierSizes() map[string]int32 {
	defaults := map[string]int32{"small": 100, "medium": 500, "large": 2000}
	sizes := map[string]int32{}
	for tier, def := range defaults {
		envKey := fmt.Sprintf("SHARD_%s_VOLUME_GB", strings.ToUpper(tier))
		if val, ok := parsePositiveInt32(os.Getenv(envKey)); ok {
			sizes[tier] = val
		} else {
			sizes[tier] = def
		}
	}
	return sizes
}

// buildDockerLauncher constructs a DockerLauncher from env vars set on
// the coordinator container by docker-compose. Fatal on missing/empty
// required vars — there's no in-process fallback in this branch, so
// the operator gets a precise error rather than silent data loss.
//
// Required env vars:
//
//	WEFT_SHARD_IMAGE      — the image tag to spawn (typically the same
//	                         image the coordinator runs from)
//	WEFT_DOCKER_NETWORK   — the docker network name from compose so the
//	                         spawned shard can resolve "coordinator"
//	                         by service DNS
//	WEFT_HOST_DATA_ROOT   — host-side absolute path bind-mounted to
//	                         WEFT_LOCAL_DATA_ROOT inside both the
//	                         coordinator and each spawned shard
//	WEFT_LOCAL_DATA_ROOT  — same volume's path inside the coordinator
//	                         container; used to mkdir per-shard subdirs
//	WEFT_COORDINATOR_ADDR — the address shards send heartbeats to;
//	                         typically "coordinator:9020"
func buildDockerLauncher(shardSecret string) *coordinator.DockerLauncher {
	requireEnv := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			log.Fatalf("docker launcher: %s is required (no in-process fallback in dev mode)", name)
		}
		return v
	}
	return &coordinator.DockerLauncher{
		Image:             requireEnv("WEFT_SHARD_IMAGE"),
		Network:           requireEnv("WEFT_DOCKER_NETWORK"),
		HostDataRoot:      requireEnv("WEFT_HOST_DATA_ROOT"),
		LocalDataRoot:     requireEnv("WEFT_LOCAL_DATA_ROOT"),
		CoordinatorAddr:   requireEnv("WEFT_COORDINATOR_ADDR"),
		HeartbeatInterval: 10 * time.Second,
		ShardSecret:       shardSecret,
	}
}

func parsePositiveInt32(s string) (int32, bool) {
	if s == "" {
		return 0, false
	}
	var v int32
	if _, err := fmt.Sscanf(s, "%d", &v); err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func parseShardsFlag(flag, shardSecret string) []shard.Shard {
	var shards []shard.Shard
	for _, spec := range strings.Split(flag, ",") {
		parts := strings.SplitN(spec, "=", 2)
		if len(parts) != 2 {
			log.Fatalf("invalid shard spec %q: expected id=addr", spec)
		}
		id, addr := parts[0], parts[1]

		gs, err := shard.NewGRPCShard(id, addr, shardSecret)
		if err != nil {
			log.Fatalf("connect to shard %s at %s: %v", id, addr, err)
		}
		shards = append(shards, gs)
		log.Printf("connected to shard %s at %s", id, addr)
	}
	return shards
}
