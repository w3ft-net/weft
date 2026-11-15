package compute

import (
	"context"
	"reflect"
	"testing"

	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// keyedShard answers any query with fixed rows of an awk aggregate's group
// query: a key and the columns beside it.
type keyedShard struct {
	shard.Shard
	id   string
	rows []map[string]string // MetaVars of each row; "raw" is the key
}

func (s *keyedShard) ID() string { return s.id }
func (s *keyedShard) QueryToSink(_ context.Context, _ string, _ []any, sink shard.RowSink) {
	for _, r := range s.rows {
		meta := map[string]string{planner.CountColumn: "1"}
		for k, v := range r {
			if k != "raw" {
				meta[k] = v
			}
		}
		sink.SendRow(&pb.ResultRow{Raw: r["raw"], MetaVars: meta}, s.id)
	}
	sink.SendSentinel(s.id)
}

type keyedResolver struct{ sh *keyedShard }

func (r keyedResolver) ShardID() string            { return r.sh.id }
func (r keyedResolver) Dial() (shard.Shard, error) { return r.sh, nil }

func runKeyedGroup(t *testing.T, render pb.GroupRender, shards ...*keyedShard) ([]string, error) {
	t.Helper()
	eng := &Engine{}
	var descs []*pb.ShardDescriptor
	for _, sh := range shards {
		eng.RegisterResolver(keyedResolver{sh})
		descs = append(descs, &pb.ShardDescriptor{ShardId: sh.id})
	}
	stream, err := eng.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
		Sql: "SELECT 1", MergeKey: "raw", Shards: descs, Group: &pb.GroupSpec{Render: render},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := DrainLineStream(context.Background(), stream)
	var got []string
	for _, l := range lines {
		got = append(got, l.Raw)
	}
	return got, err
}

func sumRow(key, sum string) map[string]string {
	return map[string]string{"raw": key, planner.SumColumn: sum}
}

func bestRow(key, best, txt, ns string) map[string]string {
	return map[string]string{"raw": key, planner.BestColumn: best, planner.BestTxtColumn: txt, planner.BestNsColumn: ns}
}

func TestKeyedAggregator_SumAddsTheShardsPerKey(t *testing.T) {
	a := &keyedShard{id: "a", rows: []map[string]string{sumRow("x", "5"), sumRow("y", "0")}}
	b := &keyedShard{id: "b", rows: []map[string]string{sumRow("x", "-7"), sumRow("z", "1000")}}
	got, err := runKeyedGroup(t, pb.GroupRender_GROUP_RENDER_KEY_SUM, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"x -2", "y 0", "z 1000"}; !reflect.DeepEqual(got, want) {
		t.Errorf("printed %q, want %q", got, want)
	}
}

// A sum is exact however large: the shards add in 128 bits and compute in
// arbitrary precision, so there is nothing to round and nothing to check. awk's
// float sum agrees below 2^53 and is the one that rounds above it.
func TestKeyedAggregator_SumIsExactPastFloatRange(t *testing.T) {
	const big = "9007199254740993" // 2^53 + 1, not a float64
	a := &keyedShard{id: "a", rows: []map[string]string{sumRow("x", big), sumRow("y", "99999999999999999999999999")}}
	b := &keyedShard{id: "b", rows: []map[string]string{sumRow("x", "1"), sumRow("y", "1")}}
	got, err := runKeyedGroup(t, pb.GroupRender_GROUP_RENDER_KEY_SUM, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"x 9007199254740994", "y 100000000000000000000000000"}; !reflect.DeepEqual(got, want) {
		t.Errorf("printed %q, want %q", got, want)
	}
}

func TestKeyedAggregator_MaxAndMinKeepTheFirstOfEqualNumbers(t *testing.T) {
	// A shard's rows arrive in key order.
	a := &keyedShard{id: "a", rows: []map[string]string{bestRow("n", "-2", "-2", "10"), bestRow("x", "5", "5.0", "30")}}
	b := &keyedShard{id: "b", rows: []map[string]string{bestRow("n", "-2", "-2.0", "40"), bestRow("x", "5", "5", "20")}}
	for render, want := range map[pb.GroupRender][]string{
		// x: equal numbers, the earlier row is shard b's; n: shard a's is earlier.
		pb.GroupRender_GROUP_RENDER_KEY_MAX: {"x 5"},
		pb.GroupRender_GROUP_RENDER_KEY_MIN: {"n -2"},
	} {
		got, err := runKeyedGroup(t, render, a, b)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s printed %q, want %q", render, got, want)
		}
	}
}

// A max keeps a key only if some value beat the 0 an unset element holds.
func TestKeyedAggregator_AKeyThatNeverBeatsZeroIsAbsent(t *testing.T) {
	a := &keyedShard{id: "a", rows: []map[string]string{bestRow("hi", "9", "9", "1"), bestRow("lo", "-4", "-4", "1"), bestRow("zero", "0", "0", "1")}}
	if got, _ := runKeyedGroup(t, pb.GroupRender_GROUP_RENDER_KEY_MAX, a); !reflect.DeepEqual(got, []string{"hi 9"}) {
		t.Errorf("max printed %q, want only hi", got)
	}
	b := &keyedShard{id: "b", rows: []map[string]string{bestRow("hi", "9", "9", "1"), bestRow("lo", "-4", "-4", "1"), bestRow("zero", "0", "0", "1")}}
	if got, _ := runKeyedGroup(t, pb.GroupRender_GROUP_RENDER_KEY_MIN, b); !reflect.DeepEqual(got, []string{"lo -4"}) {
		t.Errorf("min printed %q, want only lo", got)
	}
}

// A shard that sends something that isn't a sum is a protocol failure, and the
// query fails: a missing column is an error, never zero.
func TestKeyedAggregator_AMissingOrBadColumnIsAnError(t *testing.T) {
	for name, row := range map[string]map[string]string{
		"no sum":     {"raw": "x"},
		"text":       {"raw": "x", planner.SumColumn: "lots"},
		"a fraction": {"raw": "x", planner.SumColumn: "1.5"},
		"empty":      {"raw": "x", planner.SumColumn: ""},
	} {
		got, err := runKeyedGroup(t, pb.GroupRender_GROUP_RENDER_KEY_SUM, &keyedShard{id: "a", rows: []map[string]string{row}})
		if err == nil || len(got) != 0 {
			t.Errorf("%s: printed %q, %v; want a failure and nothing printed", name, got, err)
		}
	}
	row := bestRow("x", "5", "5", "1")
	delete(row, planner.BestColumn)
	if _, err := runKeyedGroup(t, pb.GroupRender_GROUP_RENDER_KEY_MAX, &keyedShard{id: "a", rows: []map[string]string{row}}); err == nil {
		t.Error("a max row without its number was accepted")
	}
}

func runKeyedGroupSpec(t *testing.T, spec *pb.GroupSpec, shards ...*keyedShard) ([]string, error) {
	t.Helper()
	eng := &Engine{}
	var descs []*pb.ShardDescriptor
	for _, sh := range shards {
		eng.RegisterResolver(keyedResolver{sh})
		descs = append(descs, &pb.ShardDescriptor{ShardId: sh.id})
	}
	stream, err := eng.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{Sql: "SELECT 1", MergeKey: "raw", Shards: descs, Group: spec})
	if err != nil {
		return nil, err
	}
	lines, err := DrainLineStream(context.Background(), stream)
	var got []string
	for _, l := range lines {
		got = append(got, l.Raw)
	}
	return got, err
}

// The value order sorts the aggregate's printed lines as warp's sort would, and
// head keeps the first of them.
func TestKeyedAggregator_ValueOrderMatchesSort(t *testing.T) {
	a := &keyedShard{id: "a", rows: []map[string]string{sumRow("a", "5"), sumRow("b", "5"), sumRow("c", "9"), sumRow("d", "-2")}}
	for name, c := range map[string]struct {
		spec *pb.GroupSpec
		want []string
	}{
		"ascending":         {&pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_KEY_SUM, Order: pb.GroupOrder_GROUP_ORDER_VALUE}, []string{"d -2", "a 5", "b 5", "c 9"}},
		"key reversed only": {&pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_KEY_SUM, Order: pb.GroupOrder_GROUP_ORDER_VALUE, OrderReverse: true}, []string{"c 9", "a 5", "b 5", "d -2"}},
		"both reversed":     {&pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_KEY_SUM, Order: pb.GroupOrder_GROUP_ORDER_VALUE, OrderReverse: true, OrderTieReverse: true}, []string{"c 9", "b 5", "a 5", "d -2"}},
		"head":              {&pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_KEY_SUM, Order: pb.GroupOrder_GROUP_ORDER_VALUE, OrderReverse: true, Limit: 2}, []string{"c 9", "a 5"}},
	} {
		got, err := runKeyedGroupSpec(t, c.spec, a)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: printed %q, %v; want %q", name, got, err, c.want)
		}
	}
}

// A value order or its flags where compute does not understand them fail the
// request rather than print the lines in the wrong order.
func TestGroup_ValueOrderNeedsAKeyRender(t *testing.T) {
	for _, spec := range []*pb.GroupSpec{
		{Render: pb.GroupRender_GROUP_RENDER_COUNT_LINE, Order: pb.GroupOrder_GROUP_ORDER_VALUE},
		{Render: pb.GroupRender_GROUP_RENDER_TOTAL, Order: pb.GroupOrder_GROUP_ORDER_VALUE},
		{Render: pb.GroupRender_GROUP_RENDER_KEY_SUM, OrderReverse: true},
		{Render: pb.GroupRender_GROUP_RENDER_KEY_SUM, OrderTieReverse: true},
		{Render: pb.GroupRender_GROUP_RENDER_KEY_SUM, Limit: 3},
	} {
		if _, err := runKeyedGroupSpec(t, spec, &keyedShard{id: "a"}); err == nil {
			t.Errorf("%+v was accepted", spec)
		}
	}
}

func TestGroupToProto_ValueOrder(t *testing.T) {
	got := GroupToProto(&planner.GroupSpec{Render: planner.RenderKeyCount, Limit: 4, Order: &planner.ValueOrder{Reverse: true, TieReverse: true}})
	if got.Order != pb.GroupOrder_GROUP_ORDER_VALUE || !got.OrderReverse || !got.OrderTieReverse || got.Limit != 4 {
		t.Errorf("GroupToProto = %v", got)
	}
}

// parseLeadingFloat must read what warp's sort reads: no exponent, one dot, a
// sign, blanks first, 0 for anything else.
func TestParseLeadingFloat(t *testing.T) {
	for s, want := range map[string]float64{
		"5": 5, "-5": -5, "+5": 5, " \t7": 7, "5.5": 5.5, ".5": 0.5, "5.": 5, "1e3": 1, "1.2.3": 1.2, "12abc": 12,
		"": 0, "abc": 0, "-": 0, "+": 0, ".": 0, "-.": 0, "--5": 0, "0x10": 0, "1e999": 1, "99999999999999999999999": 1e23,
	} {
		if got := parseLeadingFloat(s); got != want {
			t.Errorf("parseLeadingFloat(%q) = %v, want %v", s, got, want)
		}
	}
}
