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
	hbInterval := flag.Duration("heartbeat-interval", 60*time.Second, "heartbeat interval")
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

			// Retry initial heartbeat with backoff so we survive
			// starting simultaneously with the coordinator.
			for i := 0; i < 10; i++ {
				if sendHeartbeat(client, *shardID, myAddr, ds) {
					break
				}
				time.Sleep(time.Duration(i+1) * time.Second)
			}

			ticker := time.NewTicker(*hbInterval)
			defer ticker.Stop()
			for range ticker.C {
				sendHeartbeat(client, *shardID, myAddr, ds)
			}
		}()
		log.Printf("shard %s heartbeating to %s every %s", *shardID, *coordAddr, *hbInterval)
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

func sendHeartbeat(client pb.WeftCoordinatorClient, shardID, addr string, ds *shard.DuckDBShard) bool {
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
		return false
	}
	return true
}
