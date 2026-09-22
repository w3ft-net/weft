// loom-producer drives the synthetic Loom Inc log streams in prod.
//
// One process, one goroutine per (app, env) stream, appending to its
// own log file at the rate set by RATE_SHAPE and at the error
// proportion set by ERROR_RATIO. Files are watched by the host's
// weftd-loom@{prod,sand}.service units, which forward to the
// coordinator at ingest.w3ft.net:9020 under per-env agent tokens.
//
// Wire shape is byte-identical to dev/demo/generator.py — the
// onboarding tour reads the same content in both dev and prod.
//
// Usage:
//
//	loom-producer --config /etc/loom-producer/streams.yaml
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	weft "github.com/w3ft-net/weft-go"
)

type streamSpec struct {
	App        string `yaml:"app"`
	Env        string `yaml:"env"`
	Log        string `yaml:"log"`
	Shape      string `yaml:"shape"`
	ErrorRatio string `yaml:"error_ratio"`
}

type config struct {
	Streams []streamSpec `yaml:"streams"`
}

func main() {
	cfgPath := flag.String("config", "/etc/loom-producer/streams.yaml", "stream spec file")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("loom-producer: %v", err)
	}
	if len(cfg.Streams) == 0 {
		log.Fatalf("loom-producer: no streams in %s", *cfgPath)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Files are opened once up front into a shared map, rather than
	// each stream's own goroutine privately owning and defer-closing
	// its handle — the checkout-chain goroutines below need write
	// access to api/edge/worker's files for the same env, not just
	// their own ambient stream's file. os.File.Write under O_APPEND is
	// atomic per call for lines well under the filesystem write limit,
	// so concurrent ambient + chain writers to the same file need no
	// extra mutex.
	files := make(map[string]*os.File, len(cfg.Streams))
	for _, s := range cfg.Streams {
		if err := os.MkdirAll(filepath.Dir(s.Log), 0o755); err != nil {
			log.Fatalf("loom-producer: mkdir for %s: %v", s.Log, err)
		}
		f, err := os.OpenFile(s.Log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("loom-producer: open %s: %v", s.Log, err)
		}
		files[s.App+"@"+s.Env] = f
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()

	var wg sync.WaitGroup
	for _, s := range cfg.Streams {
		shape, err := ParseShape(s.Shape)
		if err != nil {
			log.Fatalf("loom-producer: %s/%s bad RATE_SHAPE %q: %v", s.App, s.Env, s.Shape, err)
		}
		errShape, err := ParseErrorRatio(s.ErrorRatio)
		if err != nil {
			log.Fatalf("loom-producer: %s/%s bad ERROR_RATIO %q: %v", s.App, s.Env, s.ErrorRatio, err)
		}
		log.Printf("loom-producer: %s/%s shape=%s error_ratio=%s log=%s",
			s.App, s.Env, s.Shape, s.ErrorRatio, s.Log)

		wg.Add(1)
		go func(spec streamSpec, file *os.File, shape, errShape RateShape) {
			defer wg.Done()
			runStream(ctx, spec.App, file, shape, errShape)
		}(s, files[s.App+"@"+s.Env], shape, errShape)
	}

	// Checkout chain: one goroutine per env that has api/edge/worker
	// all present, simulating a request that genuinely hops all three
	// services with a shared trace ID. See chain.go.
	for _, env := range distinctEnvs(cfg.Streams) {
		api, aok := files["api@"+env]
		edge, eok := files["edge@"+env]
		worker, wok := files["worker@"+env]
		if !aok || !eok || !wok {
			continue
		}
		wg.Add(1)
		go func(env string, api, edge, worker *os.File) {
			defer wg.Done()
			runChains(ctx, env, api, edge, worker)
		}(env, api, edge, worker)
	}

	wg.Wait()
	log.Print("loom-producer: shutdown complete")
}

// distinctEnvs returns the unique, order-stable set of envs present
// across the configured streams.
func distinctEnvs(streams []streamSpec) []string {
	seen := make(map[string]bool)
	var out []string
	for _, s := range streams {
		if !seen[s.Env] {
			seen[s.Env] = true
			out = append(out, s.Env)
		}
	}
	return out
}

func loadConfig(path string) (*config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for i, s := range c.Streams {
		if s.App == "" || s.Env == "" || s.Log == "" || s.Shape == "" {
			return nil, fmt.Errorf("%s: streams[%d] missing app/env/log/shape", path, i)
		}
		if s.ErrorRatio == "" {
			c.Streams[i].ErrorRatio = "0.05"
		}
	}
	return &c, nil
}

// runStream mirrors the Python main loop: recompute rate every 250ms,
// accumulate fractional tokens, emit one line per whole token.
//
// Each stream has its own *rand.Rand seeded from time + app + env so a
// restart doesn't replay yesterday's content but two streams under the
// same process don't share state either.
func runStream(ctx context.Context, app string, out *os.File, shape, errShape RateShape) {
	const step = 250 * time.Millisecond

	rng := newRNG(app + "@" + out.Name())
	started := time.Now()
	last := started
	var accumulator float64

	t := time.NewTicker(step)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			elapsed := now.Sub(started).Seconds()
			tick := now.Sub(last).Seconds()
			last = now

			rate := shape.At(elapsed)
			ratio := clamp01(errShape.At(elapsed))
			accumulator += rate * tick

			for accumulator >= 1.0 {
				level := pickLevel(rng, ratio)
				if _, err := fmt.Fprintln(out, LineFor(rng, app, level, weft.NewTraceID())); err != nil {
					log.Printf("loom-producer: write %s: %v", out.Name(), err)
					return
				}
				accumulator -= 1.0
			}
		}
	}
}

func clamp01(x float64) float64 {
	if x < 0 {
		return 0
	}
	if x > 1 {
		return 1
	}
	return x
}

func newRNG(salt string) *rand.Rand {
	// PCG seed pair: nanoseconds + a cheap hash of the salt string.
	// Salt comes from the per-stream identity so two streams launched
	// in the same nanosecond still diverge.
	var h uint64 = 1469598103934665603
	for i := 0; i < len(salt); i++ {
		h ^= uint64(salt[i])
		h *= 1099511628211
	}
	return rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), h))
}
