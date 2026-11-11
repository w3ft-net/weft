package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/graybeardcorp/weft/grpctoken"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

type secondCount struct {
	At time.Time
	N  int64
}

type shardQuerier interface {
	Query(ctx context.Context, sql string, args ...any) ([]*pb.ResultRow, error)
}

// shardStream collects a shard's streamed query result. weft-admin can't use
// shard.GRPCShard: the shard package links DuckDB, which this binary is built
// without (CGO_ENABLED=0).
type shardStream struct{ c pb.ShardQueryClient }

func (s shardStream) Query(ctx context.Context, sql string, args ...any) ([]*pb.ResultRow, error) {
	strs := make([]string, len(args))
	for i, a := range args {
		strs[i] = fmt.Sprint(a)
	}
	stream, err := s.c.Query(ctx, &pb.QueryRequest{Sql: sql, Args: strs})
	if err != nil {
		return nil, err
	}
	var rows []*pb.ResultRow
	for {
		row, err := stream.Recv()
		if err == io.EOF {
			return rows, nil
		}
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
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
	token := os.Getenv("SHARD_SECRET")
	conn, err := grpc.NewClient(resp.Shard.Addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainStreamInterceptor(grpctoken.ClientTokenStreamInterceptor(token)),
	)
	if err != nil {
		log.Fatalf("dial shard %s at %s: %v", id, resp.Shard.Addr, err)
	}
	defer conn.Close()
	sh := shardStream{pb.NewShardQueryClient(conn)}

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
