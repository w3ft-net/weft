package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/kevlarcon/weft/compute"
	"github.com/kevlarcon/weft/coordinator"
	coordsvc "github.com/kevlarcon/weft/coordinator/service"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
)

func main() {
	port := flag.Int("port", 9020, "gRPC listen port")
	shardsFlag := flag.String("shards", "", "comma-separated shard specs: id=addr=start=end,...")
	computeAddr := flag.String("compute", "", "compute worker address (host:port)")
	customer := flag.String("customer", "kevlar", "customer ID for row-level security")
	appsFlag := flag.String("apps", "", "comma-separated app names (e.g., vimgym,callbox)")
	flag.Parse()

	if *shardsFlag == "" {
		log.Fatal("--shards is required (e.g., shard-0=shard-0:9000=2026-03-28T10:00:00Z=2026-03-28T11:00:00Z)")
	}

	// Parse shard specs and connect.
	var shards []shard.Shard
	var apps []string
	appSet := map[string]bool{}

	for _, spec := range strings.Split(*shardsFlag, ",") {
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

		// Derive app name from shard ID for now.
		// TODO: query shard for its app list.
		if !appSet[id] {
			appSet[id] = true
		}
	}

	// Connect to compute pool.
	var pool compute.ComputePool
	if *computeAddr != "" {
		p, err := compute.NewGRPCComputePool(*computeAddr)
		if err != nil {
			log.Fatalf("connect to compute at %s: %v", *computeAddr, err)
		}
		pool = p
		log.Printf("connected to compute at %s", *computeAddr)
	} else {
		pool = &compute.LocalComputePool{Parallelism: 2}
		log.Printf("using local compute pool (parallelism=2)")
	}

	coord := &coordinator.Coordinator{
		Shards:   shards,
		Customer: *customer,
		Pool:     pool,
	}

	// App names from flag, or fall back to shard IDs.
	if *appsFlag != "" {
		apps = strings.Split(*appsFlag, ",")
	} else {
		for a := range appSet {
			apps = append(apps, a)
		}
	}

	// Start gRPC server.
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
