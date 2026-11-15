package coordinator

import (
	"testing"

	"github.com/w3ft-net/weft/shape/shapetest"

	pb "github.com/w3ft-net/weft/proto/weftpb"
	"github.com/w3ft-net/weft/shape"
)

// Gap 2 of docs/plans/tenant-scope.md: a live tail must admit exactly what the
// historical read returns, which means honouring env and every namespace of a
// multi-source read, not only the first.
func TestRowMatcher_HonoursEnvAndEveryNamespace(t *testing.T) {
	m := &RowMatcher{Coords: shape.Set{
		shapetest.In(1, shape.Coord{Namespace: shape.Value("a"), Env: shape.Value("prod")}),
		shapetest.In(1, shape.Coord{Namespace: shape.Value("b"), Env: shape.Value("prod")}),
	}}
	for _, tc := range []struct {
		row  Row
		want bool
	}{
		{Row{LogRow: &pb.LogRow{Namespace: "a", Env: "prod"}, Added: shapetest.Added(1)}, true},
		{Row{LogRow: &pb.LogRow{Namespace: "b", Env: "prod"}, Added: shapetest.Added(1)}, true},
		{Row{LogRow: &pb.LogRow{Namespace: "a", Env: "staging"}, Added: shapetest.Added(1)}, false},
		{Row{LogRow: &pb.LogRow{Namespace: "b", Env: ""}, Added: shapetest.Added(1)}, false},
		{Row{LogRow: &pb.LogRow{Namespace: "c", Env: "prod"}, Added: shapetest.Added(1)}, false},
	} {
		if got := m.Match(tc.row); got != tc.want {
			t.Errorf("Match(%+v) = %v, want %v", tc.row, got, tc.want)
		}
	}
	if len(shape.BuildLayout().Columns) > 0 && m.Match(Row{LogRow: &pb.LogRow{Namespace: "a", Env: "prod"}, Added: shapetest.Added(2)}) {
		t.Error("a row of another tenant was admitted")
	}
	if (&RowMatcher{}).Match(Row{LogRow: &pb.LogRow{}, Added: shapetest.Added(1)}) {
		t.Error("a matcher with no coordinates admits nothing")
	}
}
