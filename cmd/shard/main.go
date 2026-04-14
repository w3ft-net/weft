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

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/shard"
	shardsvc "github.com/kevlarcon/weft/shard/service"
)

func main() {
	grpcPort := flag.Int("grpc-port", 9000, "gRPC listen port")
	httpPort := flag.Int("http-port", 9001, "HTTP health listen port")
	dbPath := flag.String("db", "", "DuckDB path (empty = in-memory)")
	shardID := flag.String("id", "shard-0", "shard identifier")
	coordAddr := flag.String("coordinator-addr", "", "coordinator gRPC address for heartbeats")
	flag.Parse()

	ds, err := shard.NewDuckDBShard(*shardID, *dbPath)
	if err != nil {
		log.Fatalf("failed to open shard: %v", err)
	}
	defer ds.Close()
	log.Printf("shard %s opened (db=%s)", *shardID, *dbPath)

	// gRPC server.
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

	// Heartbeat goroutine.
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

			sendHeartbeat(client, *shardID, myAddr, ds)
			for range ticker.C {
				sendHeartbeat(client, *shardID, myAddr, ds)
			}
		}()
		log.Printf("shard %s heartbeating to %s", *shardID, *coordAddr)
	}

	// HTTP health endpoint.
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"shard_id":  *shardID,
			"row_count": ds.RowCount(),
		})
	})

	log.Printf("shard %s HTTP listening on :%d", *shardID, *httpPort)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *httpPort), nil); err != nil {
		log.Fatalf("http serve: %v", err)
	}
}

func sendHeartbeat(client pb.WeftCoordinatorClient, shardID, addr string, ds *shard.DuckDBShard) {
	req := &pb.ShardHeartbeatRequest{
		ShardId:  shardID,
		Addr:     addr,
		State:    "INGESTING",
		RowCount: ds.RowCount(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := client.Heartbeat(ctx, req); err != nil {
		log.Printf("heartbeat: %v", err)
	}
}
