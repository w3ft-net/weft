package compute

import (
	"context"
	"sync"
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// recordingShard is a shard.Shard that records the sql and args it was asked
// to run. Only QueryToSink is ever called by FanOutToShards.
type recordingShard struct {
	shard.Shard
	id  string
	got map[string]queryCall
	mu  *sync.Mutex
}

type queryCall struct {
	sql  string
	args []any
}

func (r recordingShard) QueryToSink(_ context.Context, sql string, args []any, sink shard.RowSink) {
	r.mu.Lock()
	r.got[r.id] = queryCall{sql, args}
	r.mu.Unlock()
	sink.SendSentinel(r.id)
}

type discardSink struct{}

func (discardSink) SendRow(*pb.ResultRow, string) {}
func (discardSink) Fail(string, error)            {}
func (discardSink) SendSentinel(string)           {}

func TestFanOutToShards_ShardVariantReplacesSQLAndArgs(t *testing.T) {
	got := map[string]queryCall{}
	var mu sync.Mutex
	resolve := func(_ context.Context, d *pb.ShardDescriptor) (shard.Shard, error) {
		return recordingShard{id: d.ShardId, got: got, mu: &mu}, nil
	}
	req := &pb.ExecuteQueryRequest{
		Sql:  "base",
		Args: []string{"c1"},
		Shards: []*pb.ShardDescriptor{
			{ShardId: "plain"},
			{ShardId: "typed", Sql: "optimistic", Args: []string{"c1", "extra"}},
		},
	}
	FanOutToShards(context.Background(), req, discardSink{}, resolve)

	if c := got["plain"]; c.sql != "base" || len(c.args) != 1 || c.args[0] != "c1" {
		t.Errorf("shard without a variant ran %+v, want the request's sql and args", c)
	}
	if c := got["typed"]; c.sql != "optimistic" || len(c.args) != 2 || c.args[1] != "extra" {
		t.Errorf("shard with a variant ran %+v, want its own sql and args", c)
	}
}
