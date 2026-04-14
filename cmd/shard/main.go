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
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/kevlarcon/weft/coordinator"
	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	shardsvc "github.com/kevlarcon/weft/shard/service"
)

func main() {
	grpcPort := flag.Int("grpc-port", 9000, "gRPC listen port")
	httpPort := flag.Int("http-port", 9001, "HTTP health listen port")
	dbPath := flag.String("db", "", "DuckDB path (empty = in-memory)")
	shardID := flag.String("id", "shard-0", "shard identifier")
	windowStart := flag.String("window-start", "", "window start (RFC3339)")
	windowEnd := flag.String("window-end", "", "window end (RFC3339)")
	seed := flag.String("seed", "", "seed with fixture data: shard-0 or shard-1")
	coordAddr := flag.String("coordinator-addr", "", "coordinator gRPC address for heartbeats (e.g., coordinator:9020)")
	flag.Parse()

	// Blank-start mode: if no window flags, start without DuckDB and wait
	// for the coordinator's Configure RPC to assign identity + time window.
	var ds *shard.DuckDBShard
	if *windowStart != "" && *windowEnd != "" {
		start, err := time.Parse(time.RFC3339, *windowStart)
		if err != nil {
			log.Fatalf("invalid --window-start: %v", err)
		}
		end, err := time.Parse(time.RFC3339, *windowEnd)
		if err != nil {
			log.Fatalf("invalid --window-end: %v", err)
		}

		ds, err = shard.NewDuckDBShard(*shardID, *dbPath, start, end)
		if err != nil {
			log.Fatalf("failed to open shard: %v", err)
		}
		defer ds.Close()

		if *seed != "" {
			rows := coordinator.DevShardRows(*seed)
			if rows == nil {
				log.Fatalf("unknown seed: %s (use shard-0 or shard-1)", *seed)
			}
			if err := ds.Ingest(rows); err != nil {
				log.Fatalf("seed ingest: %v", err)
			}
			log.Printf("seeded %d rows", len(rows))
		}
	} else {
		log.Println("blank-start mode: waiting for Configure RPC")
	}

	// gRPC server. Pass nil ds for blank-start (Configure RPC creates it).
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *grpcPort))
	if err != nil {
		log.Fatalf("listen grpc: %v", err)
	}
	srv := grpc.NewServer()
	pb.RegisterShardQueryServer(srv, shardsvc.NewServer(ds))

	go func() {
		log.Printf("shard %s gRPC listening on :%d", *shardID, *grpcPort)
		if err := srv.Serve(lis); err != nil {
			log.Fatalf("grpc serve: %v", err)
		}
	}()

	// Heartbeat goroutine: push state to coordinator every 60s.
	if *coordAddr != "" {
		go func() {
			hostname, _ := os.Hostname()
			myAddr := fmt.Sprintf("%s:%d", hostname, *grpcPort)

			conn, err := grpc.NewClient(*coordAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				log.Printf("heartbeat: connect to coordinator: %v", err)
				return
			}
			client := pb.NewWeftCoordinatorClient(conn)

			ticker := time.NewTicker(60 * time.Second)
			defer ticker.Stop()

			// Send first heartbeat immediately.
			sendHeartbeat(client, *shardID, myAddr, ds)

			for range ticker.C {
				sendHeartbeat(client, *shardID, myAddr, ds)
			}
		}()
		log.Printf("shard %s heartbeating to %s", *shardID, *coordAddr)
	}

	// HTTP health endpoint.
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		h := map[string]any{
			"shard_id": *shardID,
		}
		if ds != nil {
			h["state"] = "INGESTING"
			h["window_start"] = ds.WindowStart().Format(time.RFC3339)
			h["window_end"] = ds.WindowEnd().Format(time.RFC3339)
			h["row_count"] = ds.RowCount()
		} else {
			h["state"] = "UNCONFIGURED"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h)
	})

	log.Printf("shard %s HTTP listening on :%d", *shardID, *httpPort)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *httpPort), nil); err != nil {
		log.Fatalf("http serve: %v", err)
	}
}

func sendHeartbeat(client pb.WeftCoordinatorClient, shardID, addr string, ds *shard.DuckDBShard) {
	req := &pb.ShardHeartbeatRequest{
		ShardId: shardID,
		Addr:    addr,
		State:   "UNCONFIGURED",
	}
	if ds != nil {
		req.State = "INGESTING"
		req.WindowStart = ds.WindowStart().Format(time.RFC3339)
		req.WindowEnd = ds.WindowEnd().Format(time.RFC3339)
		req.RowCount = ds.RowCount()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.Heartbeat(ctx, req); err != nil {
		log.Printf("heartbeat: %v", err)
	}
}
