package compute

import (
	"context"
	"reflect"
	"strconv"
	"testing"

	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shard"
)

// groupedShard answers any query with fixed grouped rows: a distinct line
// and its count, in merge-key order, as a shard's GROUP BY returns them.
type groupedShard struct {
	shard.Shard
	id   string
	rows []groupedRow
	// bytes, when set, is the byte total of a RenderBytes shard, which
	// sends one row holding it instead of rows.
	bytes *int
	// keyed, when set, are the rows of a numeric extreme: a line and its
	// planner.SortKeyColumn, which the shard sends instead of rows.
	keyed []keyedRow
}

type keyedRow struct{ line, key string }

type groupedRow struct {
	line  string
	count int
}

func (s *groupedShard) ID() string { return s.id }
func (s *groupedShard) QueryToSink(_ context.Context, _ string, _ []any, sink shard.RowSink) {
	if s.bytes != nil {
		sink.SendRow(&pb.ResultRow{MetaVars: map[string]string{planner.CountColumn: "1", planner.BytesColumn: strconv.Itoa(*s.bytes)}}, s.id)
	}
	for _, r := range s.keyed {
		sink.SendRow(&pb.ResultRow{Raw: r.line, MetaVars: map[string]string{planner.CountColumn: "1", planner.SortKeyColumn: r.key}}, s.id)
	}
	for _, r := range s.rows {
		sink.SendRow(&pb.ResultRow{Raw: r.line, MetaVars: map[string]string{planner.CountColumn: strconv.Itoa(r.count)}}, s.id)
	}
	sink.SendSentinel(s.id)
}

type groupedResolver struct{ sh *groupedShard }

func (r groupedResolver) ShardID() string            { return r.sh.id }
func (r groupedResolver) Dial() (shard.Shard, error) { return r.sh, nil }

// TestGroup_RendersWhatTheStagesPrint: a GroupSpec prints what the stages it
// replaced print. Two shards share "b", so compute must sum their counts.
func TestGroup_RendersWhatTheStagesPrint(t *testing.T) {
	a := &groupedShard{id: "a", rows: []groupedRow{{"a", 2}, {"b", 3}}}
	b := &groupedShard{id: "b", rows: []groupedRow{{"b", 4}, {"c", 1}}}
	for _, tc := range []struct {
		name   string
		render pb.GroupRender
		want   []string
	}{
		{"wc -l", pb.GroupRender_GROUP_RENDER_TOTAL, []string{"10"}},
		{"sort -u", pb.GroupRender_GROUP_RENDER_LINE, []string{"a", "b", "c"}},
		{"sort | uniq -c", pb.GroupRender_GROUP_RENDER_COUNT_LINE, []string{"      2 a", "      7 b", "      1 c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &Engine{}
			eng.RegisterResolver(groupedResolver{a})
			eng.RegisterResolver(groupedResolver{b})
			stream, err := eng.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
				Sql: "SELECT 1", MergeKey: "raw",
				Shards: []*pb.ShardDescriptor{{ShardId: "a"}, {ShardId: "b"}},
				Group:  &pb.GroupSpec{Render: tc.render},
			})
			if err != nil {
				t.Fatal(err)
			}
			lines, err := DrainLineStream(context.Background(), stream)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, l := range lines {
				got = append(got, l.Raw)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("printed %q, want %q", got, tc.want)
			}
		})
	}
}

// The aggregate strings a coordinator sent before GroupSpec fail the query
// instead of passing the shards' per-line counts through as rows.
func TestGroup_GroupAggregateStringsAreRejected(t *testing.T) {
	for _, name := range []string{"lines", "distinct", "count_distinct", "distinct_total", "key_count"} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Engine{}).ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{Aggregate: name}); err == nil {
				t.Errorf("aggregate %q was accepted", name)
			}
		})
	}
}

// A GroupSpec this engine can't render fails the query: printing the
// shards' distinct lines as if they were the rows would be wrong output.
func TestGroup_UnspecifiedRenderFailsTheQuery(t *testing.T) {
	_, err := (&Engine{}).ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{Group: &pb.GroupSpec{}})
	if err == nil {
		t.Fatal("an unspecified render was accepted")
	}
}

func TestGroup_BothFormsFailTheQuery(t *testing.T) {
	_, err := (&Engine{}).ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
		Aggregate: planner.AggregateSpans,
		Group:     &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_TOTAL},
	})
	if err == nil {
		t.Fatal("a request naming both forms was accepted")
	}
}

// GroupToProto is what the coordinator sends: every render the planner
// produces must reach compute as the render that prints the same text.
func TestGroupToProto(t *testing.T) {
	if got := GroupToProto(nil); got != nil {
		t.Errorf("GroupToProto(nil) = %v, want nil", got)
	}
	for _, tc := range []struct {
		render planner.GroupRender
		want   pb.GroupRender
	}{
		{planner.RenderTotal, pb.GroupRender_GROUP_RENDER_TOTAL},
		{planner.RenderLine, pb.GroupRender_GROUP_RENDER_LINE},
		{planner.RenderCountLine, pb.GroupRender_GROUP_RENDER_COUNT_LINE},
		{planner.RenderKeyCount, pb.GroupRender_GROUP_RENDER_KEY_COUNT},
		{planner.RenderKeySum, pb.GroupRender_GROUP_RENDER_KEY_SUM},
		{planner.RenderKeyMax, pb.GroupRender_GROUP_RENDER_KEY_MAX},
		{planner.RenderKeyMin, pb.GroupRender_GROUP_RENDER_KEY_MIN},
	} {
		if got := GroupToProto(&planner.GroupSpec{Render: tc.render}); got.GetRender() != tc.want {
			t.Errorf("%s: render = %s, want %s", tc.render, got.GetRender(), tc.want)
		}
	}
}

// TestGroup_OrderedByCountMatchesTheStages: ordering and limiting the
// groups in compute prints exactly what `sort -rn | head -n N` printed after
// the uniq -c text, ties included. The reference is the stages themselves,
// so tie order is whatever warp's sort does, not a guess at it.
func TestGroup_OrderedByCountMatchesTheStages(t *testing.T) {
	// Equal totals across keys (b, aa and a all reach 4; c, B and the non-ASCII
	// é reach 2, which pins byte order against a locale's),
	// split over two shards so counts must be summed before ordering.
	a := &groupedShard{id: "a", rows: []groupedRow{{"B", 1}, {"a", 1}, {"aa", 4}, {"b", 3}, {"c", 2}, {"z", 9}}}
	b := &groupedShard{id: "b", rows: []groupedRow{{"B", 1}, {"a", 3}, {"b", 1}, {"d", 10}, {"e", 10}, {"é", 2}}}
	run := func(t *testing.T, req *pb.ExecuteQueryRequest) []string {
		t.Helper()
		eng := &Engine{}
		eng.RegisterResolver(groupedResolver{a})
		eng.RegisterResolver(groupedResolver{b})
		req.Sql, req.MergeKey = "SELECT 1", "raw"
		req.Shards = []*pb.ShardDescriptor{{ShardId: "a"}, {ShardId: "b"}}
		stream, err := eng.ExecuteQuery(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		lines, err := DrainLineStream(context.Background(), stream)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, l := range lines {
			out = append(out, l.Raw)
		}
		return out
	}
	for _, limit := range []int{0, 1, 2, 3, 4, 7, 50} {
		t.Run("limit "+strconv.Itoa(limit), func(t *testing.T) {
			stages := []string{"sort -rn"}
			if limit > 0 {
				stages = append(stages, "head -n "+strconv.Itoa(limit))
			}
			want := run(t, &pb.ExecuteQueryRequest{
				Group:  &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_COUNT_LINE},
				Stages: stages,
			})
			if len(want) == 0 {
				t.Fatal("reference printed nothing")
			}
			got := run(t, &pb.ExecuteQueryRequest{Group: &pb.GroupSpec{
				Render: pb.GroupRender_GROUP_RENDER_COUNT_LINE,
				Order:  pb.GroupOrder_GROUP_ORDER_COUNT_DESC,
				Limit:  int64(limit),
			}})
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ordered group printed\n%q\nthe stages printed\n%q", got, want)
			}
		})
	}
}

// An order or limit that no shape uses fails the query rather than being
// ignored: the text would otherwise be wrong without a sign of it.
func TestGroup_OrderAndLimitNeedACountedRender(t *testing.T) {
	for name, g := range map[string]*pb.GroupSpec{
		"order on a total":          {Render: pb.GroupRender_GROUP_RENDER_TOTAL, Order: pb.GroupOrder_GROUP_ORDER_COUNT_DESC},
		"order on a distinct total": {Render: pb.GroupRender_GROUP_RENDER_DISTINCT_TOTAL, Order: pb.GroupOrder_GROUP_ORDER_COUNT_DESC},
		"order on lines":            {Render: pb.GroupRender_GROUP_RENDER_LINE, Order: pb.GroupOrder_GROUP_ORDER_COUNT_DESC},
		"limit, no order":           {Render: pb.GroupRender_GROUP_RENDER_COUNT_LINE, Limit: 3},
		"negative limit":            {Render: pb.GroupRender_GROUP_RENDER_COUNT_LINE, Order: pb.GroupOrder_GROUP_ORDER_COUNT_DESC, Limit: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Engine{}).ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{Group: g}); err == nil {
				t.Error("an unusable order/limit was accepted")
			}
		})
	}
}

// TestGroup_DistinctTotalMatchesTheStages: counting distinct lines in compute
// prints what `sort -u | wc -l` printed, including 0 for no rows. Two shards
// share "b", which must count once.
func TestGroup_DistinctTotalMatchesTheStages(t *testing.T) {
	for name, rows := range map[string][2][]groupedRow{
		"overlap": {{{"a", 2}, {"b", 3}}, {{"b", 4}, {"c", 1}}},
		"one":     {{{"a", 5}}, {{"a", 7}}},
		"empty":   {nil, nil},
	} {
		t.Run(name, func(t *testing.T) {
			eng := &Engine{}
			eng.RegisterResolver(groupedResolver{&groupedShard{id: "a", rows: rows[0]}})
			eng.RegisterResolver(groupedResolver{&groupedShard{id: "b", rows: rows[1]}})
			run := func(req *pb.ExecuteQueryRequest) []string {
				t.Helper()
				req.Sql, req.MergeKey = "SELECT 1", "raw"
				req.Shards = []*pb.ShardDescriptor{{ShardId: "a"}, {ShardId: "b"}}
				stream, err := eng.ExecuteQuery(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				lines, err := DrainLineStream(context.Background(), stream)
				if err != nil {
					t.Fatal(err)
				}
				var out []string
				for _, l := range lines {
					out = append(out, l.Raw)
				}
				return out
			}
			want := run(&pb.ExecuteQueryRequest{
				Group:  &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_LINE},
				Stages: []string{"wc -l"},
			})
			if len(want) != 1 {
				t.Fatalf("reference printed %q, want one line", want)
			}
			got := run(&pb.ExecuteQueryRequest{Group: &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_DISTINCT_TOTAL}})
			if !reflect.DeepEqual(got, want) {
				t.Errorf("distinct total printed %q, the stages printed %q", got, want)
			}
		})
	}
}

// TestGroup_KeyCountPrintsAnAwkTally: the render prints `key count` for each
// distinct line, summing across shards, keys in byte order, as an awk
// `for (k in c)` tally over the same lines does. An empty key keeps its
// leading space.
func TestGroup_KeyCountPrintsAnAwkTally(t *testing.T) {
	a := &groupedShard{id: "a", rows: []groupedRow{{"", 1}, {"a", 2}, {"b", 3}}}
	b := &groupedShard{id: "b", rows: []groupedRow{{"b", 4}, {"é", 1}}}
	eng := &Engine{}
	eng.RegisterResolver(groupedResolver{a})
	eng.RegisterResolver(groupedResolver{b})
	stream, err := eng.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
		Sql: "SELECT 1", MergeKey: "raw",
		Shards: []*pb.ShardDescriptor{{ShardId: "a"}, {ShardId: "b"}},
		Group:  &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_KEY_COUNT},
	})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := DrainLineStream(context.Background(), stream)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, l := range lines {
		got = append(got, l.Raw)
	}
	if want := []string{" 1", "a 2", "b 7", "é 1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("printed %q, want %q", got, want)
	}
}

// TestGroup_BytesPrintsWcC: the render sums the shards' byte totals and
// prints one bare number, as wc -c does, and 0 when no shard had a line.
func TestGroup_BytesPrintsWcC(t *testing.T) {
	n := func(v int) *int { return &v }
	for _, tc := range []struct {
		name string
		a, b *int
		want string
	}{
		{"sums across shards", n(10), n(25), "35"},
		{"one shard empty", n(10), nil, "10"},
		{"no lines", nil, nil, "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &Engine{}
			eng.RegisterResolver(groupedResolver{&groupedShard{id: "a", bytes: tc.a}})
			eng.RegisterResolver(groupedResolver{&groupedShard{id: "b", bytes: tc.b}})
			stream, err := eng.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
				Sql: "SELECT 1", MergeKey: "raw",
				Shards: []*pb.ShardDescriptor{{ShardId: "a"}, {ShardId: "b"}},
				Group:  &pb.GroupSpec{Render: pb.GroupRender_GROUP_RENDER_BYTES},
			})
			if err != nil {
				t.Fatal(err)
			}
			lines, err := DrainLineStream(context.Background(), stream)
			if err != nil {
				t.Fatal(err)
			}
			if len(lines) != 1 || lines[0].Raw != tc.want {
				t.Errorf("printed %v, want %q", lines, tc.want)
			}
		})
	}
}

// TestGroup_NumericExtremePicksWhatSortPrints: each shard sends its one best
// line with the number sort -n compares; compute keeps the greatest (or least)
// number and breaks a tie by the line's bytes, in the same direction, as
// sort's last-resort compare does. No lines prints nothing.
func TestGroup_NumericExtremePicksWhatSortPrints(t *testing.T) {
	for _, tc := range []struct {
		name   string
		render pb.GroupRender
		a, b   []keyedRow
		want   []string
	}{
		{"max across shards", pb.GroupRender_GROUP_RENDER_NUMERIC_MAX,
			[]keyedRow{{"x 5", "5"}}, []keyedRow{{"y 7", "7"}}, []string{"y 7"}},
		{"max tie takes the greater line", pb.GroupRender_GROUP_RENDER_NUMERIC_MAX,
			[]keyedRow{{"7 b", "7"}}, []keyedRow{{"7 a", "7"}}, []string{"7 b"}},
		{"min across shards", pb.GroupRender_GROUP_RENDER_NUMERIC_MIN,
			[]keyedRow{{"x 5", "5"}}, []keyedRow{{"y -7", "-7"}}, []string{"y -7"}},
		{"min tie takes the lesser line", pb.GroupRender_GROUP_RENDER_NUMERIC_MIN,
			[]keyedRow{{"7 b", "7"}}, []keyedRow{{"7 a", "7"}}, []string{"7 a"}},
		{"keys compare as numbers, not text", pb.GroupRender_GROUP_RENDER_NUMERIC_MAX,
			[]keyedRow{{"nine", "9"}}, []keyedRow{{"ten", "10"}}, []string{"ten"}},
		{"exponent keys from the shard", pb.GroupRender_GROUP_RENDER_NUMERIC_MAX,
			[]keyedRow{{"big", "1e+21"}}, []keyedRow{{"small", "5"}}, []string{"big"}},
		{"one shard empty", pb.GroupRender_GROUP_RENDER_NUMERIC_MIN,
			[]keyedRow{{"only", "3"}}, nil, []string{"only"}},
		{"no lines", pb.GroupRender_GROUP_RENDER_NUMERIC_MAX, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := &Engine{}
			eng.RegisterResolver(groupedResolver{&groupedShard{id: "a", keyed: tc.a}})
			eng.RegisterResolver(groupedResolver{&groupedShard{id: "b", keyed: tc.b}})
			stream, err := eng.ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{
				Sql: "SELECT 1", MergeKey: "raw",
				Shards: []*pb.ShardDescriptor{{ShardId: "a"}, {ShardId: "b"}},
				Group:  &pb.GroupSpec{Render: tc.render},
			})
			if err != nil {
				t.Fatal(err)
			}
			lines, err := DrainLineStream(context.Background(), stream)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, l := range lines {
				got = append(got, l.Raw)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("printed %q, want %q", got, tc.want)
			}
		})
	}
}
