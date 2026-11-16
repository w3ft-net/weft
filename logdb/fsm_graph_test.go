package logdb

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qmuntal/stateless"
)

// TestEmitFSMGraphs regenerates the Graphviz DOT for the shard and bucket
// FSMs under docs/diagrams/. It's a generator, not an assertion, so it's
// gated behind WEFT_EMIT_FSM_DOT to stay out of normal `go test` runs.
//
//	WEFT_EMIT_FSM_DOT=1 go test ./logdb -run TestEmitFSMGraphs
//	dot -Tpng docs/diagrams/shard-fsm.dot  -o docs/diagrams/shard-fsm.png
//	dot -Tpng docs/diagrams/bucket-fsm.dot -o docs/diagrams/bucket-fsm.png
//
// Each SM is wired with stub external storage so ToGraph's initial-state
// read resolves without a Postgres fixture; only the genesis state matters
// to the diagram (LAUNCHING for shards, STASIS for buckets).
func TestEmitFSMGraphs(t *testing.T) {
	if os.Getenv("WEFT_EMIT_FSM_DOT") == "" {
		t.Skip("set WEFT_EMIT_FSM_DOT=1 to regenerate FSM diagrams")
	}
	outDir := filepath.Join("..", "docs", "diagrams")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}

	sh := &Shard{}
	sh.sm = stateless.NewStateMachineWithExternalStorage(
		stubReader(StateLaunching), stubWriter, stateless.FiringQueued,
	)
	sh.configureSM()
	writeDOT(t, filepath.Join(outDir, "shard-fsm.dot"),
		withDynamicEdges(sh.sm.ToGraph(), shardDynamicEdges))

	b := &BucketSM{Bucket: &Bucket{}}
	b.sm = stateless.NewStateMachineWithExternalStorage(
		stubReader(BucketStasis), stubWriter, stateless.FiringQueued,
	)
	b.configure()
	writeDOT(t, filepath.Join(outDir, "bucket-fsm.dot"), b.sm.ToGraph())
}

// shardDynamicEdges are the PermitDynamic transitions of the shard FSM,
// which ToGraph omits because their destinations are resolved by a guard
// at fire time rather than declared statically. We splice them in by hand
// so the rendered diagram matches the FSM table.
// Targets are read off shardsm.go:
//
//   - TriggerHeartbeat from LAUNCHING / SUSPENDED → postRecoveryTarget():
//     INGESTING for a fresh launch, or READ_ONLY / DEGRADED when the shard
//     is re-promoting to its pre-recovery state.
//   - TriggerHeartbeatStale from INGESTING / DEGRADED / READ_ONLY →
//     staleTargetFromActive(): SUSPENDED when an EBS volume survives the
//     instance, else GONE.
//
// Styled dashed so they read as distinct from the static Permit edges.
const shardDynamicEdges = `
	// dynamic (PermitDynamic) transitions — destination chosen by a guard at fire time
	edge [style=dashed];
	LAUNCHING -> INGESTING [label="heartbeat", color="#1a7f37", fontcolor="#1a7f37"];
	LAUNCHING -> READ_ONLY [label="heartbeat (post-recovery)", color="#1a7f37", fontcolor="#1a7f37"];
	LAUNCHING -> DEGRADED [label="heartbeat (post-recovery)", color="#1a7f37", fontcolor="#1a7f37"];
	SUSPENDED -> INGESTING [label="heartbeat", color="#1a7f37", fontcolor="#1a7f37"];
	SUSPENDED -> READ_ONLY [label="heartbeat (post-recovery)", color="#1a7f37", fontcolor="#1a7f37"];
	SUSPENDED -> DEGRADED [label="heartbeat (post-recovery)", color="#1a7f37", fontcolor="#1a7f37"];
	INGESTING -> SUSPENDED [label="heartbeat-stale (EBS survives)", color="#cf222e", fontcolor="#cf222e"];
	INGESTING -> GONE [label="heartbeat-stale (no volume)", color="#cf222e", fontcolor="#cf222e"];
	DEGRADED -> SUSPENDED [label="heartbeat-stale (EBS survives)", color="#cf222e", fontcolor="#cf222e"];
	DEGRADED -> GONE [label="heartbeat-stale (no volume)", color="#cf222e", fontcolor="#cf222e"];
	READ_ONLY -> SUSPENDED [label="heartbeat-stale (EBS survives)", color="#cf222e", fontcolor="#cf222e"];
	READ_ONLY -> GONE [label="heartbeat-stale (no volume)", color="#cf222e", fontcolor="#cf222e"];
`

// withDynamicEdges splices extra DOT statements in before the closing brace
// of a ToGraph() digraph.
func withDynamicEdges(dot, edges string) string {
	i := strings.LastIndex(dot, "}")
	if i < 0 {
		return dot + edges
	}
	return dot[:i] + edges + dot[i:]
}

func stubReader(s stateless.State) func(context.Context) (stateless.State, error) {
	return func(context.Context) (stateless.State, error) { return s, nil }
}

func stubWriter(context.Context, stateless.State) error { return nil }

func writeDOT(t *testing.T, path, dot string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(dot), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("wrote %s (%d bytes)", path, len(dot))
}
