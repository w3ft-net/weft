// Package tailfanout is benchmark/decision-support code for
// docs/plans/coordinator-horizontal-scaling.md item 4: it fans tail -f
// rows out across multiple coordinator processes over Redis Pub/Sub,
// topic-scoped to (namespace, service, kind) so per-node overhead tracks
// that node's own subscriber interest rather than total cluster ingest
// volume. A gRPC-mesh candidate was benchmarked here too, lost on latency
// and backpressure-vs-N scaling, and was deleted once Redis won.
//
// This package has no production caller — coordinator.TailFanout
// (coordinator/tail_fanout.go) is production's independent copy of the
// same design, not an import of this package. This one remains as an
// ongoing perf-regression harness (bench/tailfanout_bench_test.go).
package tailfanout

import (
	"context"
	"sync"

	"github.com/graybeardcorp/weft/coordinator"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// Topic is the routing key both candidates fan rows out by — the
// structural (non-content) portion of a coordinator.RowMatcher. It
// intentionally excludes RowMatcher.Patterns: content-grep filtering can't
// be turned into a routing key and stays applied locally, post-delivery,
// by the real TailBroker on whichever node a row lands on.
type Topic struct {
	Namespace, Service, Kind string
	// Wildcard is true for the empty-Namespace "warp.log" case: match
	// every row regardless of the other fields.
	Wildcard bool
}

// topicFor derives a Topic from a subscription's RowMatcher.
func topicFor(m *coordinator.RowMatcher) Topic {
	// The benchmark subscribes one tuple at a time.
	t := m.Coords[0]
	ns, ok := t.Namespace.Literal()
	if !ok {
		return Topic{Wildcard: true}
	}
	svc, _ := t.Service.Literal()
	kind, _ := t.Kind.Literal()
	return Topic{Namespace: ns, Service: svc, Kind: kind}
}

// Transport moves rows between Nodes for topics a peer has announced
// interest in. It does not know about local subscribers or RowMatcher
// content-pattern filtering — that stays entirely inside the real
// coordinator.TailBroker each Node wraps.
type Transport interface {
	// AnnounceInterest tells peers this node's local subscriber count for
	// t went from 0 to 1 (add=true) or from 1 to 0 (add=false).
	AnnounceInterest(t Topic, add bool)
	// Publish sends rows that originated on this node to any peer that has
	// announced interest in their topic.
	Publish(rows []*pb.LogRow)
	// SetDeliverFunc registers the callback invoked when a row arrives
	// from a peer. Called once, before the transport starts accepting
	// connections.
	SetDeliverFunc(func(rows []*pb.LogRow))
	Close() error
}

// Node simulates one coordinator process's tail-fanout surface: a real,
// unmodified TailBroker for local delivery, plus a pluggable Transport for
// cross-node topic-scoped delivery.
type Node struct {
	ID     string
	Broker *coordinator.TailBroker
	tr     Transport

	mu   sync.Mutex
	refs map[Topic]int
}

// NewNode wires broker and tr together: rows the transport delivers from a
// peer are broadcast into the local broker exactly as ingestBatch does for
// locally-ingested rows, so a peer-delivered row goes through the same
// RowMatcher.Match (including content-pattern filtering) as a local one.
func NewNode(id string, tr Transport) *Node {
	broker := coordinator.NewTailBroker()
	n := &Node{
		ID:     id,
		Broker: broker,
		tr:     tr,
		refs:   make(map[Topic]int),
	}
	tr.SetDeliverFunc(broker.Broadcast)
	return n
}

// Subscribe registers a local tail subscription and, on the first
// subscriber for a topic, announces interest to peers.
func (n *Node) Subscribe(ctx context.Context, matcher *coordinator.RowMatcher) (uint64, <-chan *pb.LogRow) {
	id, ch := n.Broker.Subscribe(ctx, matcher)

	t := topicFor(matcher)
	n.mu.Lock()
	n.refs[t]++
	first := n.refs[t] == 1
	n.mu.Unlock()

	if first {
		n.tr.AnnounceInterest(t, true)
	}
	return id, ch
}

// Unsubscribe mirrors Subscribe: it removes the local subscription and, if
// that was the last local subscriber for matcher's topic, withdraws
// interest from peers.
//
// matcher must be the same RowMatcher (or an equal one) passed to the
// matching Subscribe call — Node needs it to know which topic's refcount
// to decrement; TailBroker.Unsubscribe itself only takes the id.
func (n *Node) Unsubscribe(id uint64, matcher *coordinator.RowMatcher) {
	n.Broker.Unsubscribe(id)

	t := topicFor(matcher)
	n.mu.Lock()
	n.refs[t]--
	last := n.refs[t] <= 0
	if last {
		delete(n.refs, t)
	}
	n.mu.Unlock()

	if last {
		n.tr.AnnounceInterest(t, false)
	}
}

// Publish delivers rows to this node's own local subscribers (mirroring
// ingestBatch's Tail.Broadcast call) and fans them out to interested
// peers.
func (n *Node) Publish(rows []*pb.LogRow) {
	n.Broker.Broadcast(rows)
	n.tr.Publish(rows)
}

// Close releases the node's transport resources.
func (n *Node) Close() error {
	return n.tr.Close()
}
