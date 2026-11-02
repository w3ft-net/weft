package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// swapArchive points an ARCHIVED shard at a converted copy of its archive, the
// last step of scripts/convert-archives.py. It reads the shard first and stops
// unless it is at oldPath (or already at newPath), and without yes it only says
// what it would do.
func swapArchive(ctx context.Context, client pb.WeftAdminClient, shardID, oldPath, newPath string, yes bool, out io.Writer) error {
	resp, err := client.GetShard(ctx, &pb.GetShardRequest{ShardId: shardID})
	if err != nil {
		return fmt.Errorf("get shard %s: %w", shardID, err)
	}
	cur := resp.GetShard().GetParquetPath()
	fmt.Fprintf(out, "shard %s: state=%s parquet=%q\n", shardID, resp.GetShard().GetState(), cur)
	switch cur {
	case newPath:
		fmt.Fprintf(out, "already at the new path; nothing to swap\n")
		return nil
	case oldPath:
	default:
		return fmt.Errorf("shard %s is at %q, not at %q: refusing to swap", shardID, cur, oldPath)
	}
	if !yes {
		fmt.Fprintf(out, "dry run: would swap to %s and tombstone the old archive (re-run with --yes)\n", newPath)
		return nil
	}
	if _, err := client.SwapShardArchive(ctx, &pb.SwapShardArchiveRequest{ShardId: shardID, OldPath: oldPath, NewPath: newPath}); err != nil {
		return fmt.Errorf("swap %s: %w", shardID, err)
	}
	fmt.Fprintf(out, "shard %s swapped to %s\n", shardID, newPath)
	return nil
}

// swapArchiveCmd is `weft-admin swap-archive`, run on a coordinator.
func swapArchiveCmd(args []string) {
	fs := flag.NewFlagSet("swap-archive", flag.ExitOnError)
	coord := fs.String("coord", "127.0.0.1:9022", "coordinator WeftAdmin gRPC address (localhost-only; SharedToken auth)")
	shardID := fs.String("shard", "", "the shard to swap")
	oldPath := fs.String("old", "", "the shard's archive path now (the swap is refused if it is elsewhere)")
	newPath := fs.String("new", "", "the converted archive's path")
	yes := fs.Bool("yes", false, "actually swap (default: say what would change)")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *shardID == "" || *oldPath == "" || *newPath == "" {
		fmt.Fprintln(os.Stderr, "usage: weft-admin swap-archive --shard ID --old PATH --new PATH [--yes]")
		os.Exit(2)
	}
	client, closeConn, err := newAdminClient(*coord)
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()
	ctx, cancel := context.WithTimeout(adminCtx(context.Background()), time.Minute)
	defer cancel()
	if err := swapArchive(ctx, client, *shardID, *oldPath, *newPath, *yes, os.Stdout); err != nil {
		log.Fatal(err)
	}
}
