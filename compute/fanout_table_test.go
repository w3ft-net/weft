package compute

import (
	"context"
	"sync"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// A plan that names the table holding its rows
// has it passed to each shard that can use it. A shard that cannot (a cold
// shard, one from before the table field) is queried as it always was.

type tableCall struct {
	table, sql, fieldSQL string
	args                 []any
}

type tableShard struct {
	recordingShard
	tables map[string]tableCall
}

func (s tableShard) QueryTableToSink(_ context.Context, table, sql, fieldSQL string, args []any, sink shard.RowSink) {
	s.mu.Lock()
	s.tables[s.id] = tableCall{table, sql, fieldSQL, args}
	s.mu.Unlock()
	sink.SendSentinel(s.id)
}

func TestFanOutToShards_TheNamedTableReachesAShardThatTakesIt(t *testing.T) {
	plain := map[string]queryCall{}
	tables := map[string]tableCall{}
	var mu sync.Mutex
	resolve := func(_ context.Context, d *pb.ShardDescriptor) (shard.Shard, error) {
		rs := recordingShard{id: d.ShardId, got: plain, mu: &mu}
		if d.ShardId == "cold" {
			return rs, nil // no TableQuerier
		}
		return tableShard{recordingShard: rs, tables: tables}, nil
	}
	req := &pb.ExecuteQueryRequest{
		Sql:      "base",
		FieldSql: "fields",
		Table:    "l_abc",
		Args:     []string{"c1"},
		Shards: []*pb.ShardDescriptor{
			{ShardId: "live"},
			{ShardId: "typed", Sql: "optimistic", FieldSql: "optimistic fields", Args: []string{"c1", "extra"}},
			{ShardId: "cold"},
		},
	}
	FanOutToShards(context.Background(), req, discardSink{}, resolve)

	if c := tables["live"]; c.table != "l_abc" || c.sql != "base" || c.fieldSQL != "fields" || len(c.args) != 1 {
		t.Errorf("live shard got %+v", c)
	}
	if c := tables["typed"]; c.table != "l_abc" || c.sql != "optimistic" || c.fieldSQL != "optimistic fields" || len(c.args) != 2 {
		t.Errorf("shard with a variant got %+v, want its own sql with the same table", c)
	}
	if _, ok := tables["cold"]; ok {
		t.Error("a shard without TableQuerier was asked for a table")
	}
	if c := plain["cold"]; c.sql != "base" {
		t.Errorf("cold shard ran %+v, want the plain sql", c)
	}
}

func TestFanOutToShards_NoTableMeansNoTableCall(t *testing.T) {
	plain := map[string]queryCall{}
	tables := map[string]tableCall{}
	var mu sync.Mutex
	resolve := func(_ context.Context, d *pb.ShardDescriptor) (shard.Shard, error) {
		return tableShard{recordingShard: recordingShard{id: d.ShardId, got: plain, mu: &mu}, tables: tables}, nil
	}
	req := &pb.ExecuteQueryRequest{Sql: "base", Shards: []*pb.ShardDescriptor{{ShardId: "live"}}}
	FanOutToShards(context.Background(), req, discardSink{}, resolve)

	if len(tables) != 0 {
		t.Errorf("a request with no table made table calls: %+v", tables)
	}
	if c := plain["live"]; c.sql != "base" {
		t.Errorf("ran %+v, want the plain query", c)
	}
}
