package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/weftd"
)

const (
	defaultCheckpointDir  = "/var/lib/weftd/checkpoints"
	defaultFlushInterval  = 5 * time.Second
	defaultCommitInterval = 15 * time.Second
)

// CheckpointConfig is the `checkpoint:` block. Every field is optional;
// older agents ignore the block entirely (yaml.Unmarshal is not strict), so
// rollout order does not matter.
type CheckpointConfig struct {
	Dir            string        `yaml:"dir"`              // default /var/lib/weftd/checkpoints
	Enabled        *bool         `yaml:"enabled"`          // nil = on (same pattern as auto_update)
	FlushInterval  time.Duration `yaml:"flush_interval"`   // how often offsets are persisted
	CommitInterval time.Duration `yaml:"commit_interval"`  // how often unacked rows are acked
	MaxResumeBytes string        `yaml:"max_resume_bytes"` // backlog replay cap per file, e.g. 256MiB
}

func (c CheckpointConfig) enabled() bool { return c.Enabled == nil || *c.Enabled }

// configureCheckpointing opens the checkpoint store and applies the delivery
// settings to the package-level shipper state. It never fails the agent: a
// store that cannot be opened (read-only state dir, another weftd holding the
// lock) leaves checkpoints nil and file tailing behaves as it did before
// checkpointing existed. Returns the flush interval for the store's Run loop.
func configureCheckpointing(c CheckpointConfig) time.Duration {
	flush := defaultFlushInterval
	if c.FlushInterval > 0 {
		flush = c.FlushInterval
	}
	if c.CommitInterval > 0 {
		shipCfg.commitInterval = c.CommitInterval
	}
	if !c.enabled() {
		log.Printf("checkpoint: disabled by config; files are tailed from EOF on every start")
		return flush
	}

	dir := c.Dir
	if dir == "" {
		dir = defaultCheckpointDir
	}
	opts := weftd.CheckpointOptions{}
	if c.MaxResumeBytes != "" {
		n, err := parseByteSize(c.MaxResumeBytes)
		if err != nil {
			log.Printf("checkpoint: max_resume_bytes %q: %v — using the default", c.MaxResumeBytes, err)
		} else {
			opts.MaxResumeBytes = n
		}
	}
	store, err := weftd.OpenCheckpointStore(dir, opts)
	switch {
	case errors.Is(err, weftd.ErrCheckpointLocked):
		log.Printf("checkpoint: %s is locked by another weftd — running with checkpointing disabled", dir)
	case err != nil:
		log.Printf("checkpoint: cannot use %s: %v — running with checkpointing disabled", dir, err)
	default:
		checkpoints = store
		log.Printf("checkpoint: state in %s", dir)
	}
	return flush
}

// runCheckpointStatsReporter logs, once a minute and per source, the resume
// decisions that mean data was skipped or replayed, so gaps are visible.
func runCheckpointStatsReporter(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		for path, c := range checkpoints.Stats() {
			log.Printf("checkpoint stats [%s]: resumed=%d new_file=%d rotated_while_down=%d truncated=%d skipped_bytes=%d",
				path, c.Resumed, c.NewFile, c.RotatedWhileDown, c.Truncated, c.SkippedBytes)
		}
	}
}

// parseByteSize reads "1048576", "64KiB", "256MiB" or "1GiB".
func parseByteSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	mult := int64(1)
	for suffix, m := range map[string]int64{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30} {
		if strings.HasSuffix(s, suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, suffix)), m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("want a positive size like 256MiB")
	}
	return n * mult, nil
}
