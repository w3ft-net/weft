package compute

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shard"
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

func sumRow(key, sum, abs string) map[string]string {
	return map[string]string{"raw": key, planner.SumColumn: sum, planner.AbsSumColumn: abs, planner.BadColumn: "0"}
}

func bestRow(key, best, txt, ns string) map[string]string {
	return map[string]string{"raw": key, planner.BestColumn: best, planner.BestTxtColumn: txt, planner.BestNsColumn: ns, planner.BadColumn: "0"}
}

func TestKeyedAggregator_SumAddsTheShardsPerKey(t *testing.T) {
	a := &keyedShard{id: "a", rows: []map[string]string{sumRow("x", "5", "5"), sumRow("y", "-0", "0")}}
	b := &keyedShard{id: "b", rows: []map[string]string{sumRow("x", "-7", "7"), sumRow("z", "1e3", "1e3")}}
	got, err := runKeyedGroup(t, pb.GroupRender_GROUP_RENDER_KEY_SUM, a, b)
	if err != nil {
		t.Fatal(err)
	}
	// awk prints a whole number as an integer, and -0 as 0.
	if want := []string{"x -2", "y 0", "z 1000"}; !reflect.DeepEqual(got, want) {
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

// A value the shard could not vouch for, or a sum too large to be exact, fails
// the query and prints nothing, not even the keys before it.
func TestKeyedAggregator_InexactPrintsNothingAndSaysSo(t *testing.T) {
	bad := sumRow("y", "3", "3")
	bad[planner.BadColumn] = "1"
	for name, rows := range map[string][]map[string]string{
		"a bad row":       {sumRow("x", "1", "1"), bad},
		"a sum past 2^52": {sumRow("x", "1", "1"), sumRow("y", "1", "5e15")},
	} {
		got, err := runKeyedGroup(t, pb.GroupRender_GROUP_RENDER_KEY_SUM, &keyedShard{id: "a", rows: rows})
		var inexact *InexactAggregateError
		if !errors.As(err, &inexact) {
			t.Errorf("%s: error %v, want an InexactAggregateError", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: printed %q before failing", name, got)
		}
	}
}

func TestKeyedAggregator_AMissingColumnIsAnErrorNotZero(t *testing.T) {
	row := sumRow("x", "1", "1")
	delete(row, planner.AbsSumColumn)
	_, err := runKeyedGroup(t, pb.GroupRender_GROUP_RENDER_KEY_SUM, &keyedShard{id: "a", rows: []map[string]string{row}})
	var inexact *InexactAggregateError
	if err == nil || errors.As(err, &inexact) {
		t.Errorf("error %v, want a plain failure for a shard that sent no %s", err, planner.AbsSumColumn)
	}
}
