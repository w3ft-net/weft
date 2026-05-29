// weft-admin is the coordinator-resident CLI for operator/deploy actions
// that drive the shard FSM out-of-band of normal cluster operation. It dials
// the local coordinator's gRPC and calls one of the admin RPCs.
//
// The intended invocation is `kevcons ssh --command "sudo /opt/app/bin/weft-admin …"`
// against the coordinator instance during a deploy or an ad-hoc operator
// action. Localhost trust boundary; no auth.
//
// Phase 1 (this commit): roll-shard <id> only — fires RelaunchShard and
// exits. The FSM drives SUSPENDED → LAUNCHING → INGESTING on its own ticks;
// the operator confirms via shard_records.
//
// Phase 2 will add roll-cohort/roll-all + a wait-and-verify poll loop.
// Phase 3 adds skip-if-current via --target-sha against shard_records.binary_sha.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func main() {
	log.SetFlags(0) // operator-facing; timestamps just noise
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "roll-shard":
		rollShard(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: weft-admin <subcommand> [flags]

Subcommands:
  roll-shard <shard-id>   relaunch a single shard in place (binary upgrade)

Flags (apply to subcommands that take them):
  -coord     coordinator gRPC address (default "localhost:9020")
  -timeout   RPC deadline (default 30s)`)
	os.Exit(2)
}

func rollShard(args []string) {
	fs := flag.NewFlagSet("roll-shard", flag.ExitOnError)
	coord := fs.String("coord", "localhost:9020", "coordinator gRPC address")
	timeout := fs.Duration("timeout", 30*time.Second, "RPC deadline")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "roll-shard requires exactly one positional argument: <shard-id>")
		os.Exit(2)
	}
	shardID := fs.Arg(0)

	conn, err := grpc.NewClient(*coord, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial coordinator at %s: %v", *coord, err)
	}
	defer conn.Close()
	client := pb.NewWeftCoordinatorClient(conn)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if _, err := client.RelaunchShard(ctx, &pb.RelaunchShardRequest{ShardId: shardID}); err != nil {
		log.Fatalf("relaunch %s: %v", shardID, err)
	}
	fmt.Printf("relaunch fired for %s — the FSM will drive SUSPENDED → reap → LAUNCHING → INGESTING on subsequent ticks.\n", shardID)
	fmt.Println("Watch shard_records.state (and coordinator.log) to confirm the cycle completes.")
}
