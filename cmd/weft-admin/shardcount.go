package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

type secondCount struct {
	At time.Time
	N  int64
}

type shardQuerier interface {
	Query(ctx context.Context, sql string, args ...any) ([]*pb.ResultRow, error)
}

// countRunByTime counts the rows of one roll-canary run stored on a single
// shard, per second of the rows' send time.
func countRunByTime(ctx context.Context, sh shardQuerier, service, run string) ([]secondCount, error) {
	rows, err := sh.Query(ctx,
		`SELECT CAST(count(*) AS VARCHAR) AS raw, date_trunc('second', ts) AS ts
		 FROM logs WHERE service = ? AND raw LIKE ? GROUP BY 2 ORDER BY 2`,
		service, "%run="+run+" %")
	if err != nil {
		return nil, err
	}
	out := make([]secondCount, 0, len(rows))
	for _, r := range rows {
		n, err := strconv.ParseInt(r.Raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("count %q: %w", r.Raw, err)
		}
		out = append(out, secondCount{time.Unix(0, r.TsUnixNs).UTC(), n})
	}
	return out, nil
}

// shardCountCmd prints how many rows of a canary run one shard holds, per
// second, so a roll test can show which shard took the writes when.
func shardCountCmd(args []string) {
	fs := flag.NewFlagSet("shard-count", flag.ExitOnError)
	coord := fs.String("coord", "127.0.0.1:9022", "coordinator WeftAdmin gRPC address (localhost-only; SharedToken auth)")
	if err := fs.Parse(args); err != nil || fs.NArg() != 3 {
		fmt.Fprintln(os.Stderr, "usage: weft-admin shard-count <shard-id> <service> <run>")
		os.Exit(2)
	}
	id, service, run := fs.Arg(0), fs.Arg(1), fs.Arg(2)

	ctx := context.Background()
	client, closeConn, err := newAdminClient(*coord)
	if err != nil {
		log.Fatal(err)
	}
	defer closeConn()
	resp, err := client.GetShard(adminCtx(ctx), &pb.GetShardRequest{ShardId: id})
	if err != nil {
		log.Fatalf("get shard %s: %v", id, err)
	}
	sh, err := shard.NewGRPCShard(id, resp.Shard.Addr, os.Getenv("SHARD_SECRET"))
	if err != nil {
		log.Fatal(err)
	}
	defer sh.Close()

	counts, err := countRunByTime(ctx, sh, service, run)
	if err != nil {
		log.Fatalf("count on %s: %v", id, err)
	}
	var total int64
	for _, c := range counts {
		fmt.Printf("%s %d\n", c.At.Format("15:04:05"), c.N)
		total += c.N
	}
	fmt.Printf("total %d\n", total)
}
