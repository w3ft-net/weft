package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync"

	pb "github.com/kevlarcon/weft/proto/weftpb"
)

func runTails(ctx context.Context, cfg *Config, report *Report) {
	if cfg.Tails <= 0 {
		return
	}

	var wg sync.WaitGroup

	for i := 0; i < cfg.Tails; i++ {
		wg.Add(1)
		app := rand.Intn(3)
		pipeline := fmt.Sprintf("tail -f app-%d.log", app)

		go func(pipeline string) {
			defer wg.Done()
			tailSubscribe(ctx, cfg.Client, pipeline)
		}(pipeline)
	}

	wg.Wait()
}

func tailSubscribe(ctx context.Context, client pb.WeftCoordinatorClient, pipeline string) {
	stream, err := client.Query(ctx, &pb.CoordinatorQueryRequest{
		Pipeline: pipeline,
		Follow:   true,
	})
	if err != nil {
		return
	}

	for {
		_, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			return
		}
		if err != nil {
			return
		}
	}
}
