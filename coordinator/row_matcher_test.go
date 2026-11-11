package coordinator

import (
	"testing"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

// Gap 2 of docs/plans/tenant-scope.md: a live tail must admit exactly what the
// historical read returns, which means honouring env and every namespace of a
// multi-source read, not only the first.
func TestRowMatcher_HonoursEnvAndEveryNamespace(t *testing.T) {
	m := &RowMatcher{Coords: shape.Set{
		{Customer: 1, Namespace: shape.Value("a"), Env: shape.Value("prod")},
		{Customer: 1, Namespace: shape.Value("b"), Env: shape.Value("prod")},
	}}
	for _, tc := range []struct {
		row  Row
		want bool
	}{
		{Row{LogRow: &pb.LogRow{Namespace: "a", Env: "prod"}, Added: shape.Added{Customer: 1}}, true},
		{Row{LogRow: &pb.LogRow{Namespace: "b", Env: "prod"}, Added: shape.Added{Customer: 1}}, true},
		{Row{LogRow: &pb.LogRow{Namespace: "a", Env: "staging"}, Added: shape.Added{Customer: 1}}, false},
		{Row{LogRow: &pb.LogRow{Namespace: "b", Env: ""}, Added: shape.Added{Customer: 1}}, false},
		{Row{LogRow: &pb.LogRow{Namespace: "c", Env: "prod"}, Added: shape.Added{Customer: 1}}, false},
		{Row{LogRow: &pb.LogRow{Namespace: "a", Env: "prod"}, Added: shape.Added{Customer: 2}}, false},
	} {
		if got := m.Match(tc.row); got != tc.want {
			t.Errorf("Match(%+v) = %v, want %v", tc.row, got, tc.want)
		}
	}
	if (&RowMatcher{}).Match(Row{LogRow: &pb.LogRow{}, Added: shape.Added{Customer: 1}}) {
		t.Error("a matcher with no coordinates admits nothing")
	}
}
