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
	"runtime"
	"strings"

	"google.golang.org/grpc"

	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/coordinator"
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
	shardASG := flag.String("shard-asg", "", "shard ASG name for AWS discovery (legacy single-tier)")
	shardASGSmall := flag.String("shard-asg-small", "", "small tier shard ASG name")
	shardASGMedium := flag.String("shard-asg-medium", "", "medium tier shard ASG name")
	shardASGLarge := flag.String("shard-asg-large", "", "large tier shard ASG name")
	computeAddr := flag.String("compute", "", "compute worker address (host:port)")
	computeASG := flag.String("compute-asg", "", "compute ASG name for AWS discovery (production)")
	awsRegion := flag.String("region", "us-east-2", "AWS region for ASG discovery")
	customer := flag.String("customer", "", "fallback customer for dev/docker (queries pass customer per-request)")
	dbDSN := flag.String("db-dsn", "", "PostgreSQL DSN (e.g., host=localhost dbname=weft user=weft password=...)")
	flag.Parse()

	var shards []shard.Shard

	// Shard setup: explicit specs, legacy single ASG, or tiered ASGs.
	if *shardsFlag != "" {
		shards = parseShardsFlag(*shardsFlag)
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
			pool = &compute.LocalComputePool{Parallelism: 2}
		} else {
			p, err := compute.NewGRPCComputePool(addrs[0])
			if err != nil {
				log.Fatalf("connect to compute: %v", err)
			}
			pool = p
			log.Printf("connected to compute at %s (from ASG)", addrs[0])
		}
	} else {
		pool = &compute.LocalComputePool{Parallelism: 2}
		log.Printf("using local compute pool (parallelism=2)")
	}

	coord := coordinator.NewCoordinator(shards, *customer, pool)
	coord.Tail = coordinator.NewTailBroker()
	coord.Ingest = coordinator.NewRingBuffer(1_000_000) // 1M row buffer
	coord.Usage = coordinator.NewCustomerUsageStats()

	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer drainCancel()
	coord.StartDrain(drainCtx)

	// Build tiered ASG map. Fall back to legacy single --shard-asg.
	shardTiers := make(map[string]string)
	if *shardASGSmall != "" {
		shardTiers["small"] = *shardASGSmall
	} else if *shardASG != "" {
		shardTiers["small"] = *shardASG // legacy: single ASG = small tier
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

	if len(shardTiers) > 0 {
		// Production: ASG launcher for shard EC2 instances.
		asgLauncher, err := coordinator.NewASGLauncher(*awsRegion, shardTiers, 9000)
		if err != nil {
			log.Printf("warning: ASG launcher unavailable: %v", err)
		} else {
			provConfig := coordinator.DefaultProvisionerConfig()
			provConfig.S3Config = shard.S3ConfigFromEnv()
			provConfig.ParquetDir = os.Getenv("WEFT_PARQUET_DIR")
			provConfig.LambdaFunctionURL = os.Getenv("WEFT_LAMBDA_FUNCTION_URL")
			prov := coordinator.NewProvisioner(coord.Registry, asgLauncher, provConfig)
			coord.Provisioner = prov
			prov.Start(provCtx)
			log.Printf("provisioner: started with ASG tiers %v", shardTiers)
		}
	} else {
		// Dev/Docker: local in-process DuckDB shards managed by provisioner.
		launcher := coordinator.NewLocalLauncher()
		provConfig := coordinator.DevProvisionerConfig()
		provConfig.ParquetDir = os.Getenv("WEFT_PARQUET_DIR")
		prov := coordinator.NewProvisioner(coord.Registry, launcher, provConfig)
		coord.Provisioner = prov
		prov.Start(provCtx)
		log.Printf("provisioner: started with local launcher (dev mode)")
	}

	// Config store (PostgreSQL).
	dsn := *dbDSN
	if dsn == "" {
		dsn = os.Getenv("WEFT_DB_DSN")
	}
	if dsn == "" {
		dsn = "host=localhost dbname=weft user=weft password=weft sslmode=disable"
		log.Println("WARNING: using default DB DSN — set --db-dsn or WEFT_DB_DSN in production")
	}
	db, err := store.Open(dsn)
	if err != nil {
		log.Printf("warning: config store unavailable: %v", err)
	} else {
		defer db.Close()

		// Start usage metering flush (every 5 minutes).
		coord.StartUsageFlush(drainCtx, db)

		// Give the provisioner access to the store for heartbeat adoption.
		if coord.Provisioner != nil {
			coord.Provisioner.SetStore(db)
		}

		// Seed dev data when running with a customer flag (dev/docker mode).
		if *customer != "" {
			cust, _ := db.FindOrCreateCustomer(*customer)
			if cust != nil {
				db.SeedDevSubscription(cust.Name)
				db.SeedDevAgentToken(cust.ID, "dev-agent", *customer)
			}
		}

		// Assign known apps to all shards so queries route correctly.
		var allApps []string
		if appNames, err := db.AllAppNames(); err == nil {
			allApps = appNames
		}
		if len(allApps) > 0 {
			for _, entry := range coord.Registry.Entries() {
				entry.Apps = allApps
			}
			log.Printf("coordinator: assigned apps %v to %d shards", allApps, coord.Registry.Count())
		}

		// Restore archived shards from the registry.
		coord.Registry.SetStore(db)
		s3cfg := shard.S3ConfigFromEnv()
		lambdaURL := os.Getenv("WEFT_LAMBDA_FUNCTION_URL")

		if err := coord.Registry.LoadFromStore(func(rec store.ShardRecord) (shard.Shard, error) {
			if rec.State != string(coordinator.StateArchived) || rec.ParquetPath == "" {
				return nil, fmt.Errorf("skip non-archived shard %s", rec.ID)
			}
			var apps []string
			json.Unmarshal([]byte(rec.Apps), &apps)

			if lambdaURL != "" {
				return shard.NewLambdaColdShard(rec.ID, apps, lambdaURL, rec.ParquetPath), nil
			}
			return shard.NewColdShard(rec.ID, apps, rec.ParquetPath, s3cfg)
		}); err != nil {
			log.Printf("warning: load archived shards: %v", err)
		} else {
			log.Printf("coordinator: restored archived shards from store")
		}

		// Start CloudWatch poller for configured sources.
		p := poller.New(db, coord)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p.Start(ctx)

		// Start cron executor for scheduled watches.
		cronExec := &coordinator.CronExecutor{
			Store:    db,
			Coord:    coord,
			Notifier: coordinator.NewNotifier(db),
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

	// gRPC server.
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterWeftCoordinatorServer(srv, &coordsvc.Server{
		Coord: coord,
		Store: db,
	})

	log.Printf("coordinator listening on :%d (%d shards, customer=%s)", *port, len(shards), *customer)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func parseShardsFlag(flag string) []shard.Shard {
	var shards []shard.Shard
	for _, spec := range strings.Split(flag, ",") {
		parts := strings.SplitN(spec, "=", 2)
		if len(parts) != 2 {
			log.Fatalf("invalid shard spec %q: expected id=addr", spec)
		}
		id, addr := parts[0], parts[1]

		gs, err := shard.NewGRPCShard(id, addr)
		if err != nil {
			log.Fatalf("connect to shard %s at %s: %v", id, addr, err)
		}
		shards = append(shards, gs)
		log.Printf("connected to shard %s at %s", id, addr)
	}
	return shards
}
