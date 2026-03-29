package main

import (
	"flag"
	"fmt"
	"log"
	"net"

	"google.golang.org/grpc"

	"github.com/kevlarcon/weft/compute"
	computesvc "github.com/kevlarcon/weft/compute/service"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func main() {
	port := flag.Int("port", 9010, "gRPC listen port")
	parallelism := flag.Int("parallelism", 2, "warp parallelism per job")
	flag.Parse()

	pool := &compute.LocalComputePool{Parallelism: *parallelism}
	srv := grpc.NewServer()
	pb.RegisterWeftComputeServer(srv, computesvc.NewServer(pool))

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("compute worker listening on :%d (parallelism=%d)", *port, *parallelism)
	if err := srv.Serve(lis); err != nil {
		log.Fatalf("serve: %v", err)
	}
}
