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
	"github.com/kevlarcon/weft/coordinator"
	"github.com/kevlarcon/weft/coordinator/discovery"
	coordsvc "github.com/kevlarcon/weft/coordinator/service"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

func main() {
	port := flag.Int("port", 9020, "gRPC listen port")
	healthPort := flag.Int("health-port", 9021, "HTTP health listen port")
	shardsFlag := flag.String("shards", "", "comma-separated shard specs: id=addr=start=end,... (for Docker/dev)")
	shardASG := flag.String("shard-asg", "", "shard ASG name for AWS discovery (production)")
	computeAddr := flag.String("compute", "", "compute worker address (host:port)")
	computeASG := flag.String("compute-asg", "", "compute ASG name for AWS discovery (production)")
	awsRegion := flag.String("region", "us-east-2", "AWS region for ASG discovery")
	customer := flag.String("customer", "kevlar", "customer ID for row-level security")
	appsFlag := flag.String("apps", "", "comma-separated app names (e.g., vimgym,callbox)")
	flag.Parse()

	var shards []shard.Shard
	var apps []string

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

		// Re-discover periodically in a background goroutine.
		// TODO: update the coordinator's shard list on rediscovery.
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

	coord := &coordinator.Coordinator{
		Shards:   shards,
		Customer: *customer,
		Pool:     pool,
	}

	if *appsFlag != "" {
		apps = strings.Split(*appsFlag, ",")
	}

	// HTTP health endpoint.
	go func() {
		http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status": "ok",
				"shards": len(shards),
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
		Apps:  apps,
	})

	log.Printf("coordinator listening on :%d (%d shards, customer=%s)", *port, len(shards), *customer)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
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
