package compute

import (
	"context"
	"reflect"
	"strconv"
	"testing"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
)

// groupedShard answers any query with fixed grouped rows: a distinct line
// and its count, in merge-key order, as a shard's GROUP BY returns them.
type groupedShard struct {
	shard.Shard
	id   string
	rows []groupedRow
}

type groupedRow struct {
	line  string
	count int
}

func (s *groupedShard) ID() string { return s.id }
func (s *groupedShard) QueryToSink(_ context.Context, _ string, _ []any, sink shard.RowSink) {
	for _, r := range s.rows {
		sink.SendRow(&pb.ResultRow{Raw: r.line, MetaVars: map[string]string{planner.CountColumn: strconv.Itoa(r.count)}}, s.id)
	}
	sink.SendSentinel(s.id)
}

type groupedResolver struct{ sh *groupedShard }

func (r groupedResolver) ShardID() string            { return r.sh.id }
func (r groupedResolver) Dial() (shard.Shard, error) { return r.sh, nil }

// TestGroup_RendersWhatTheLegacyAggregateDoes: a request carrying a
// GroupSpec prints exactly what the same shape's legacy aggregate string
// prints. Two shards share "b", so compute must sum their counts.
func TestGroup_RendersWhatTheLegacyAggregateDoes(t *testing.T) {
	a := &groupedShard{id: "a", rows: []groupedRow{{"a", 2}, {"b", 3}}}
	b := &groupedShard{id: "b", rows: []groupedRow{{"b", 4}, {"c", 1}}}
	for _, tc := range []struct {
		name   string
		legacy string
		render pb.GroupRender
		want   []string
	}{
		{"wc -l", legacyLines, pb.GroupRender_GROUP_RENDER_TOTAL, []string{"10"}},
		{"sort -u", legacyDistinct, pb.GroupRender_GROUP_RENDER_LINE, []string{"a", "b", "c"}},
		{"sort | uniq -c", legacyCountDistinct, pb.GroupRender_GROUP_RENDER_COUNT_LINE,
			[]string{"      2 a", "      7 b", "      1 c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(req *pb.ExecuteQueryRequest) []string {
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
			legacy := run(&pb.ExecuteQueryRequest{Aggregate: tc.legacy})
			if !reflect.DeepEqual(legacy, tc.want) {
				t.Fatalf("legacy aggregate printed %q, want %q", legacy, tc.want)
			}
			if got := run(&pb.ExecuteQueryRequest{Group: &pb.GroupSpec{Render: tc.render}}); !reflect.DeepEqual(got, legacy) {
				t.Errorf("group printed %q, legacy printed %q", got, legacy)
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
		Aggregate: legacyLines,
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
		"order on a total": {Render: pb.GroupRender_GROUP_RENDER_TOTAL, Order: pb.GroupOrder_GROUP_ORDER_COUNT_DESC},
		"order on lines":   {Render: pb.GroupRender_GROUP_RENDER_LINE, Order: pb.GroupOrder_GROUP_ORDER_COUNT_DESC},
		"limit, no order":  {Render: pb.GroupRender_GROUP_RENDER_COUNT_LINE, Limit: 3},
		"negative limit":   {Render: pb.GroupRender_GROUP_RENDER_COUNT_LINE, Order: pb.GroupOrder_GROUP_ORDER_COUNT_DESC, Limit: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := (&Engine{}).ExecuteQuery(context.Background(), &pb.ExecuteQueryRequest{Group: g}); err == nil {
				t.Error("an unusable order/limit was accepted")
			}
		})
	}
}
