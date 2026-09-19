package store

import (
	"log"
	"time"
)

// LogEvaluateEvent emits one structured line per bucket/shard FSM Evaluate
// call inside evaluateStateMachines, so per-shard convergence timing under
// real N=2 load can be checked without attaching a profiler
// (docs/plans/coordinator-scaling-live-testing.md phase 3). kind is
// "bucket" or "shard"; id is that bucket/shard's own id. err is nil for a
// clean Evaluate.
func LogEvaluateEvent(kind, id string, dur time.Duration, err error) {
	if err != nil {
		log.Printf("evaluate_event kind=%s id=%s duration_ms=%d err=%q", kind, id, dur.Milliseconds(), err)
		return
	}
	log.Printf("evaluate_event kind=%s id=%s duration_ms=%d", kind, id, dur.Milliseconds())
}

// LogEvaluateTick emits one line per evaluateStateMachines tick with the
// tick's total duration and how many buckets/shards it covered, so a slow
// or stalled tick is visible without summing every per-shard
// LogEvaluateEvent line by hand — this is the line that would confirm or
// disprove the known sequential-evaluation gap (one slow shard blocking
// convergence fleet-wide, docs/known-issues.md) under real load.
func LogEvaluateTick(buckets, shards int, dur time.Duration) {
	log.Printf("evaluate_tick buckets=%d shards=%d duration_ms=%d", buckets, shards, dur.Milliseconds())
}

// LogTailFanoutEvent emits one structured line per Redis TailFanout health
// event — subscribe/publish errors, and connection establishment (which,
// after the first, means go-redis reconnected after a drop) — so the
// cross-coordinator tail -f path, previously only smoke-tested against a
// local docker-compose Valkey, is observable in prod
// (docs/plans/coordinator-scaling-live-testing.md phase 3). op is a short
// verb (connect, psubscribe, punsubscribe, publish, unmarshal). err is nil
// for a clean event such as "connect".
func LogTailFanoutEvent(op string, err error) {
	if err != nil {
		log.Printf("tailfanout_event op=%s err=%q", op, err)
		return
	}
	log.Printf("tailfanout_event op=%s", op)
}
