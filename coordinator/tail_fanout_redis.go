package coordinator

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"sync"

	weftsdk "github.com/w3ft-net/weft-go"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// redisFanout is TailFanout's optional Redis Pub/Sub piece. Topic-scoped
// PSUBSCRIBE/channel-naming is unchanged from the validated
// bench/tailfanout candidate.
//
// Known gaps, carried over from the bench source as documented-not-solved
// rather than regressions introduced here: a node holding two overlapping
// pattern subscriptions at once would receive a matching row once per
// matching pattern (Redis's own PSUBSCRIBE semantics); namespace/service/
// kind values containing Redis glob metacharacters (*, ?, [) aren't
// escaped.
type redisFanout struct {
	client *redis.Client
	ps     *redis.PubSub

	mu       sync.Mutex
	patterns map[TailTopic]string
}

var _ TailBus = (*redisFanout)(nil)

// NewRedisTailBus is the TailBus over Redis Pub/Sub at addr. It deliberately does not Ping at construction: tail is
// already documented as best-effort, so a transient ElastiCache blip at
// coordinator boot must not crash-loop the process that also owns the
// durable ingest path. go-redis's client is lazy/auto-reconnecting.
func NewRedisTailBus(addr string, tlsEnabled bool) TailBus {
	opts := &redis.Options{Addr: addr}
	if tlsEnabled {
		opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	// OnConnect fires every time go-redis (re-)establishes the underlying
	// connection — the first call is the initial connect, every call after
	// that is a reconnect following a drop. There's no separate reconnect
	// callback in the client, so this is the only hook into that signal.
	opts.OnConnect = func(ctx context.Context, cn *redis.Conn) error {
		LogTailFanoutEvent(ctx, "connect", nil)
		return nil
	}
	client := redis.NewClient(opts)
	ps := client.PSubscribe(context.Background()) // patterns added via announceInterest
	return &redisFanout{client: client, ps: ps, patterns: make(map[TailTopic]string)}
}

// Start begins receiving.
func (r *redisFanout) Start(deliver func(rows []Row)) { go r.recvLoop(deliver) }

func (r *redisFanout) recvLoop(deliver func(rows []Row)) {
	for msg := range r.ps.Channel() {
		row, err := rowFromMessage(msg.Channel, []byte(msg.Payload))
		if err != nil {
			weftsdk.Infof(context.Background(), "tailfanout: redis message on %q: %v", msg.Channel, err)
			LogTailFanoutEvent(context.Background(), "unmarshal", err)
			continue
		}
		deliver([]Row{row})
	}
}

// rowFromMessage reads one published tail row. Its group is the added columns
// the publisher named its channel by (see channelFor), never anything in the
// payload: the coordinator carries the group beside the row, not inside it.
// A channel that names no group is refused rather than read as the zero one.
func rowFromMessage(channel string, payload []byte) (Row, error) {
	added, err := addedFromChannel(channel)
	if err != nil {
		return Row{}, err
	}
	row := &pb.LogRow{}
	if err := proto.Unmarshal(payload, row); err != nil {
		return Row{}, err
	}
	return Row{row, added}, nil
}

// addedFromChannel is the inverse of channelPrefix: the partition values that
// follow "tail:" in a channel name, as many as the build's added columns.
func addedFromChannel(channel string) (shape.Added, error) {
	rest, ok := strings.CutPrefix(channel, "tail:")
	if !ok {
		return shape.Added{}, fmt.Errorf("not a tail channel")
	}
	n := len(shape.Added{}.Coord().PartitionValues())
	parts := strings.SplitN(rest, ":", n+1)
	if len(parts) < n+1 {
		return shape.Added{}, fmt.Errorf("channel names %d partition values, want %d", len(parts)-1, n)
	}
	return shape.AddedFromPartition(parts[:n])
}

// pattern builds the PSUBSCRIBE glob for a topic. Always goes through
// PSUBSCRIBE, even for a fully-specified topic, to keep one code path
// instead of branching between SUBSCRIBE/PSUBSCRIBE.
func pattern(t TailTopic) string {
	p := "tail:" + channelPrefix(t.Added)
	switch {
	case t.Wildcard:
		return p + "*"
	case t.Service == "":
		return p + t.Namespace + ":*"
	case t.Kind == "":
		return p + t.Namespace + ":" + t.Service + ":*"
	default:
		return p + t.Namespace + ":" + t.Service + ":" + t.Kind + "*"
	}
}

// channelPrefix is the partition's part of a channel name: its values, each
// followed by a colon, or nothing for a build with no partition columns. For
// the hosted shape it is "7:", so a channel is named as the previous build's
// peers name it.
func channelPrefix(a shape.Added) string {
	var b strings.Builder
	for _, v := range a.Coord().PartitionValues() {
		b.WriteString(v)
		b.WriteByte(':')
	}
	return b.String()
}

// channelFor is the concrete publish-side channel name for a row. Kind
// resolves to its "main" default, matching the proto's own documented
// default (proto/weftpb/weft.proto's LogRow.kind comment).
func channelFor(row Row) string {
	kind := row.Kind
	if kind == "" {
		kind = "main"
	}
	return "tail:" + channelPrefix(row.Added) + row.Namespace + ":" + row.Service + ":" + kind
}

func (r *redisFanout) Interest(ctx context.Context, t TailTopic, add bool) {
	p := pattern(t)
	if add {
		r.mu.Lock()
		r.patterns[t] = p
		r.mu.Unlock()
		if err := r.ps.PSubscribe(ctx, p); err != nil {
			weftsdk.Infof(ctx, "tailfanout: redis psubscribe %s: %v", p, err)
			LogTailFanoutEvent(ctx, "psubscribe", err)
		}
		return
	}
	r.mu.Lock()
	delete(r.patterns, t)
	r.mu.Unlock()
	if err := r.ps.PUnsubscribe(ctx, p); err != nil {
		weftsdk.Infof(ctx, "tailfanout: redis punsubscribe %s: %v", p, err)
		LogTailFanoutEvent(ctx, "punsubscribe", err)
	}
}

// publish pipelines one PUBLISH per row so a multi-row batch costs one
// Redis round trip.
func (r *redisFanout) Publish(rows []Row) {
	if len(rows) == 0 {
		return
	}
	ctx := context.Background()
	pipe := r.client.Pipeline()
	for _, row := range rows {
		data, err := proto.Marshal(row.LogRow)
		if err != nil {
			weftsdk.Infof(ctx, "tailfanout: redis marshal row: %v", err)
			LogTailFanoutEvent(ctx, "marshal", err)
			continue
		}
		pipe.Publish(ctx, channelFor(row), data)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		weftsdk.Infof(ctx, "tailfanout: redis publish: %v", err)
		LogTailFanoutEvent(ctx, "publish", err)
	}
}
