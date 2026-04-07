package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/configstore"
	"github.com/kevlarcon/weft/coordinator"
	"github.com/kevlarcon/weft/coordinator/discovery"
	"github.com/kevlarcon/weft/coordinator/poller"
	coordsvc "github.com/kevlarcon/weft/coordinator/service"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
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
	customer := flag.String("customer", "kevlar", "customer ID for row-level security")
	dataDir := flag.String("data-dir", "/opt/app/data", "directory for SQLite config database")
	flag.Parse()

	var shards []shard.Shard

	// Shard setup: either explicit specs or ASG discovery.
	if *shardsFlag != "" {
		shards = parseShardsFlag(*shardsFlag)
	} else if *shardASG != "" {
		disc, err := discovery.NewASGDiscovery(*awsRegion, *shardASG, 9000)
		if err != nil {
			log.Fatalf("init shard discovery: %v", err)
		}
		discovered, err := disc.DiscoverShards(context.Background())
		if err != nil {
			log.Fatalf("discover shards: %v", err)
		}
		shards = discovered
		log.Printf("discovered %d shards from ASG %s", len(shards), *shardASG)
	} else {
		log.Fatal("either --shards or --shard-asg is required")
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

	// Start background shard re-discovery for each configured ASG.
	for _, asgName := range shardTiers {
		disc, err := discovery.NewASGDiscovery(*awsRegion, asgName, 9000)
		if err != nil {
			log.Printf("warning: cannot create shard re-discovery for %s: %v", asgName, err)
			continue
		}
		go rediscoverLoop(disc, coord)
	}

	// Start provisioner with ASG launcher if tiers are configured.
	if len(shardTiers) > 0 {
		asgLauncher, err := coordinator.NewASGLauncher(*awsRegion, shardTiers, 9000)
		if err != nil {
			log.Printf("warning: ASG launcher unavailable: %v", err)
		} else {
			prov := coordinator.NewProvisioner(coord.Registry, asgLauncher, coordinator.DefaultProvisionerConfig())
			coord.Provisioner = prov
			provCtx, provCancel := context.WithCancel(context.Background())
			defer provCancel()
			prov.Start(provCtx)
			log.Printf("provisioner: started with tiers %v", shardTiers)
		}
	}

	// Config store (SQLite on EBS).
	store, err := configstore.Open(*dataDir + "/weft.db")
	if err != nil {
		log.Printf("warning: config store unavailable: %v", err)
	} else {
		defer store.Close()

		// Assign known apps to all shards so queries route correctly.
		var allApps []string
		if agentApps, err := store.AgentAppNames(); err == nil {
			allApps = append(allApps, agentApps...)
		}
		if pollerApps, err := store.AppNames(); err == nil {
			for _, a := range pollerApps {
				found := false
				for _, existing := range allApps {
					if existing == a {
						found = true
						break
					}
				}
				if !found {
					allApps = append(allApps, a)
				}
			}
		}
		if len(allApps) > 0 {
			for _, entry := range coord.Registry.Entries() {
				entry.Apps = allApps
			}
			log.Printf("coordinator: assigned apps %v to %d shards", allApps, coord.Registry.Count())
		}

		// Start CloudWatch poller for configured sources.
		p := poller.New(store, coord)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		p.Start(ctx)
	}

	// HTTP health endpoint.
	go func() {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"shards": len(coord.GetShards()),
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
		Store: store,
	})

	log.Printf("coordinator listening on :%d (%d shards, customer=%s)", *port, len(shards), *customer)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

// rediscoverLoop periodically re-discovers shards from the ASG and updates
// the coordinator's shard list. This handles instance replacements, scale-outs,
// and recovery from transient discovery failures at startup.
func rediscoverLoop(disc *discovery.ASGDiscovery, coord *coordinator.Coordinator) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		discovered, err := disc.DiscoverShards(context.Background())
		if err != nil {
			log.Printf("shard re-discovery failed: %v", err)
			continue
		}
		current := coord.GetShards()
		if shardsChanged(current, discovered) {
			log.Printf("shard re-discovery: %d -> %d shards", len(current), len(discovered))
			coord.SetShards(discovered)
		}
	}
}

// shardsChanged returns true if the discovered shard set differs from the current one.
func shardsChanged(current, discovered []shard.Shard) bool {
	if len(current) != len(discovered) {
		return true
	}
	ids := make(map[string]bool, len(current))
	for _, s := range current {
		ids[s.ID()] = true
	}
	for _, s := range discovered {
		if !ids[s.ID()] {
			return true
		}
	}
	return false
}

func parseShardsFlag(flag string) []shard.Shard {
	var shards []shard.Shard
	for _, spec := range strings.Split(flag, ",") {
		parts := strings.SplitN(spec, "=", 4)
		if len(parts) != 4 {
			log.Fatalf("invalid shard spec %q: expected id=addr=start=end", spec)
		}
		id, addr := parts[0], parts[1]
		start, err := time.Parse(time.RFC3339, parts[2])
		if err != nil {
			log.Fatalf("invalid start time %q: %v", parts[2], err)
		}
		end, err := time.Parse(time.RFC3339, parts[3])
		if err != nil {
			log.Fatalf("invalid end time %q: %v", parts[3], err)
		}

		gs, err := shard.NewGRPCShard(id, addr, start, end)
		if err != nil {
			log.Fatalf("connect to shard %s at %s: %v", id, addr, err)
		}
		shards = append(shards, gs)
		log.Printf("connected to shard %s at %s (%s to %s)", id, addr,
			start.Format("15:04"), end.Format("15:04"))
	}
	return shards
}
