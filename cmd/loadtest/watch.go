package main

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

func runWatches(ctx context.Context, cfg *Config, report *Report) {
	if cfg.Watches <= 0 {
		return
	}

	var wg sync.WaitGroup

	for i := 0; i < cfg.Watches; i++ {
		wg.Add(1)
		app := rand.Intn(3)
		pipeline := fmt.Sprintf("cat app-%d.log | grep ERROR | wc -l", app)

		go func(id int, pipeline string) {
			defer wg.Done()
			watchLoop(ctx, cfg, id, pipeline)
		}(i, pipeline)
	}

	wg.Wait()
}

func watchLoop(ctx context.Context, cfg *Config, id int, pipeline string) {
	// Stagger start to avoid thundering herd.
	jitter := time.Duration(rand.Intn(60000)) * time.Millisecond
	select {
	case <-ctx.Done():
		return
	case <-time.After(jitter):
	}

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	// Run immediately.
	execQuery(ctx, cfg.Client, pipeline)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			execQuery(ctx, cfg.Client, pipeline)
		}
	}
}
