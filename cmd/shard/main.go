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
			myIP := getPrivateIP()
			myAddr := fmt.Sprintf("%s:%d", myIP, *grpcPort)

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

// getPrivateIP returns the instance's private IP. Tries EC2 instance
// metadata first (works on EC2), falls back to os.Hostname().
func getPrivateIP() string {
	// EC2 IMDSv2: get token, then query local-ipv4.
	client := &http.Client{Timeout: 2 * time.Second}
	tokenReq, _ := http.NewRequest("PUT", "http://169.254.169.254/latest/api/token", nil)
	tokenReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	if tokenResp, err := client.Do(tokenReq); err == nil {
		defer tokenResp.Body.Close()
		token := make([]byte, 256)
		n, _ := tokenResp.Body.Read(token)
		if n > 0 {
			ipReq, _ := http.NewRequest("GET", "http://169.254.169.254/latest/meta-data/local-ipv4", nil)
			ipReq.Header.Set("X-aws-ec2-metadata-token", string(token[:n]))
			if ipResp, err := client.Do(ipReq); err == nil {
				defer ipResp.Body.Close()
				ip := make([]byte, 64)
				if n, _ := ipResp.Body.Read(ip); n > 0 {
					return string(ip[:n])
				}
			}
		}
	}
	// Fallback: hostname (works in Docker).
	hostname, _ := os.Hostname()
	return hostname
}
