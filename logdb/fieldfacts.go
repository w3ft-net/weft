package logdb

import (
	"context"
	"sync"

	weftsdk "github.com/w3ft-net/weft-go"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
	"github.com/graybeardcorp/weft/store"
)

// fieldFactSet is what a shard has told the coordinator about its field
// values (docs/plans/field-types.md). A locator is typed only if the shard
// said so and has not since retracted it; absence means unknown, which plans
// as today. Retractions are kept as untyped entries so a stale duplicate of
// the typed beat can't resurrect a fact. It has its own lock: the heartbeat
// writes it while queries read it, and neither should wait on the entry's mu.
type fieldFactSet struct {
	mu      sync.RWMutex
	entries map[shard.FactKey]factEntry
}

type factEntry struct {
	typed bool
	epoch uint64
}

// FieldTyped reports whether this shard has said every row of the key's
// source held a number at the key's locator.
func (sh *Shard) FieldTyped(k shard.FactKey) bool {
	sh.facts.mu.RLock()
	defer sh.facts.mu.RUnlock()
	return sh.facts.entries[k].typed
}

// foldFieldFacts applies a heartbeat's deltas and returns what changed: the
// facts that became typed and the ones that stopped being. A delta not newer
// than what the shard already has is ignored.
func (sh *Shard) foldFieldFacts(deltas []*pb.FieldFactDelta) (typed, dropped []store.ShardFieldFact) {
	sh.facts.mu.Lock()
	defer sh.facts.mu.Unlock()
	if sh.facts.entries == nil {
		sh.facts.entries = map[shard.FactKey]factEntry{}
	}
	for _, d := range deltas {
		k := shard.FactKey{CustomerID: d.CustomerId, Namespace: d.Namespace, Service: d.Service,
			Kind: d.Kind, Env: d.Env, Locator: d.Locator}
		cur, seen := sh.facts.entries[k]
		if seen && d.Epoch <= cur.epoch {
			continue
		}
		row := store.ShardFieldFact{ShardID: sh.ID, CustomerID: k.CustomerID, Namespace: k.Namespace,
			Service: k.Service, Kind: k.Kind, Env: k.Env, Locator: k.Locator, Epoch: int64(d.Epoch)}
		switch d.State {
		case pb.FieldFactState_FIELD_FACT_STATE_TYPED:
			sh.facts.entries[k] = factEntry{typed: true, epoch: d.Epoch}
			typed = append(typed, row)
		case pb.FieldFactState_FIELD_FACT_STATE_DIRTY:
			sh.facts.entries[k] = factEntry{typed: false, epoch: d.Epoch}
			if cur.typed {
				dropped = append(dropped, row)
			}
			weftsdk.Infof(context.Background(), "registry: shard %s: %d/%s/%s/%s env=%s %s is no longer all numbers (epoch %d)",
				sh.ID, k.CustomerID, k.Namespace, k.Service, k.Kind, k.Env, k.Locator, d.Epoch)
		}
	}
	return typed, dropped
}

// replaceFieldFacts makes the typed facts exactly rows, the store's view.
func (sh *Shard) replaceFieldFacts(rows []store.ShardFieldFact) {
	m := make(map[shard.FactKey]factEntry, len(rows))
	for _, r := range rows {
		m[shard.FactKey{CustomerID: r.CustomerID, Namespace: r.Namespace, Service: r.Service,
			Kind: r.Kind, Env: r.Env, Locator: r.Locator}] = factEntry{typed: true, epoch: uint64(r.Epoch)}
	}
	sh.facts.mu.Lock()
	sh.facts.entries = m
	sh.facts.mu.Unlock()
}

// FoldFieldFacts records a heartbeat's field facts on shard id's entry and in
// the store, and returns the epoch to acknowledge: the highest in deltas, or 0
// if the store write failed, so the shard sends them again. A shard the
// registry doesn't hold gets 0.
func (r *ShardRegistry) FoldFieldFacts(id string, deltas []*pb.FieldFactDelta) uint64 {
	entry := r.Get(id)
	if entry == nil || len(deltas) == 0 {
		return 0
	}
	typed, dropped := entry.foldFieldFacts(deltas)
	if r.store != nil && len(typed)+len(dropped) > 0 {
		if err := r.store.ApplyShardFieldFacts(id, typed, dropped); err != nil {
			weftsdk.Errorf(context.Background(), "registry: persist field facts for %s: %v", id, err)
			return 0
		}
	}
	var acked uint64
	for _, d := range deltas {
		acked = max(acked, d.Epoch)
	}
	return acked
}

// fieldFactsByShard loads every shard's typed facts and groups them by
// shard_id. The bool is false on a store error (logged), so callers leave
// the facts they hold rather than forget them.
func (r *ShardRegistry) fieldFactsByShard() (map[string][]store.ShardFieldFact, bool) {
	out := map[string][]store.ShardFieldFact{}
	if r.store == nil {
		return out, true
	}
	rows, err := r.store.ListShardFieldFacts()
	if err != nil {
		weftsdk.Infof(context.Background(), "registry: ListShardFieldFacts: %v", err)
		return out, false
	}
	for _, f := range rows {
		out[f.ShardID] = append(out[f.ShardID], f)
	}
	return out, true
}
