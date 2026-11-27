//go:build test

package logdb

import (
	"context"
	"testing"
	"time"

	"github.com/w3ft-net/weft/backends/mem"
)

// The open build runs the control plane over a shard store: a bucket and
// the shard the bucket FSM mints are persisted there, and a restarted registry
// hydrates from it, with no Postgres anywhere.
func TestControlPlane_RunsOverTheLocalShardStore(t *testing.T) {
	st := mem.NewShards()

	reg := NewShardRegistry()
	reg.SetStore(st)
	prov := NewProvisioner(reg, nil, ProvisionerConfig{MaxConcurrentEvaluate: 1})
	prov.SetShardStore(st)

	// The store comes with the default bucket; nothing here creates it.
	seeded, ok, err := st.GetBucketByLabel("default")
	if err != nil || !ok {
		t.Fatalf("the store has no default bucket: ok=%v err=%v", ok, err)
	}
	b := &seeded
	prov.LoadStateMachines()
	if len(prov.bucketSMs) != 1 {
		t.Fatalf("%d bucket SMs, want 1", len(prov.bucketSMs))
	}

	ws := time.Now().UTC().Truncate(time.Hour)
	id, err := prov.MintShard(context.Background(), MintSpec{BucketID: b.ID, Tier: "small", WindowStart: ws, WindowEnd: ws.Add(time.Hour)})
	if err != nil {
		t.Fatalf("MintShard: %v", err)
	}
	row, ok, err := st.GetShard(id)
	if err != nil || !ok || row.State != StateLaunching || row.BucketID != b.ID || row.HeartbeatToken == "" {
		t.Fatalf("minted shard in the store: %+v ok=%v err=%v", row, ok, err)
	}

	// A restart: a fresh registry over the same store adopts the shard.
	reg2 := NewShardRegistry()
	reg2.SetStore(st)
	if err := reg2.LoadFromStore(noReconstruct); err != nil {
		t.Fatal(err)
	}
	if e := reg2.Get(id); e == nil || e.State != StateLaunching {
		t.Fatalf("shard %s after restart: %+v", id, e)
	}
}
