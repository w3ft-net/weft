package coordinator

import (
	"context"
	"testing"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
	"github.com/w3ft-net/weft/shape/shapetest"
)

// The added columns of a row survive every place the coordinator carries them,
// whichever shape the build has: with no partition column they are the empty
// value, and everything still works.
func TestAdded_SurvivesTheTailTheDLQAndTheKey(t *testing.T) {
	added := shapetest.Added(1)
	lr := &pb.LogRow{Namespace: "weft", Service: "coordinator", Raw: "x"}

	// the live tail: a subscription to the group's rows gets the row
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := NewTailBroker()
	_, ch := b.Subscribe(ctx, &RowMatcher{Coords: shapetest.Group(1)})
	if !b.HasSubscriber(Row{LogRow: lr, Added: added}) {
		t.Error("the broker does not see a subscriber for the group's row")
	}
	b.Broadcast([]Row{{LogRow: lr, Added: added}})
	select {
	case got := <-ch:
		if got.Added != added || got.Raw != "x" {
			t.Errorf("the tail delivered %+v with %+v, want the row with %+v", got.LogRow, got.Added, added)
		}
	default:
		t.Fatal("the tail did not deliver the group's row to its subscription")
	}

	// the key a matcher sees, and the partition a shard is told
	if k := (Row{LogRow: lr, Added: added}).key(); !shapetest.Group(1).Matches(&k) {
		t.Errorf("the key %+v of the group's row is not in the group's scope", k)
	}
	if got, want := added.Coord().PartitionValues(), shapetest.Partition(1); len(got) != len(want) {
		t.Errorf("partition values %v, want %v", got, want)
	}
}

// A shape with a required column has no group until they are set.
func TestNoGroup_FollowsTheShapesRequiredColumns(t *testing.T) {
	if got, want := NoGroup(shape.Added{}), len(shape.BuildLayout().Columns) > 0; got != want {
		t.Errorf("NoGroup(zero) = %v, want %v", got, want)
	}
	if NoGroup(shapetest.Added(1)) {
		t.Error("a group's columns name no group")
	}
}
