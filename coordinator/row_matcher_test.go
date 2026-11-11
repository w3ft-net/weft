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
		row  *pb.LogRow
		want bool
	}{
		{&pb.LogRow{CustomerId: 1, Namespace: "a", Env: "prod"}, true},
		{&pb.LogRow{CustomerId: 1, Namespace: "b", Env: "prod"}, true},
		{&pb.LogRow{CustomerId: 1, Namespace: "a", Env: "staging"}, false},
		{&pb.LogRow{CustomerId: 1, Namespace: "b", Env: ""}, false},
		{&pb.LogRow{CustomerId: 1, Namespace: "c", Env: "prod"}, false},
		{&pb.LogRow{CustomerId: 2, Namespace: "a", Env: "prod"}, false},
	} {
		if got := m.Match(tc.row); got != tc.want {
			t.Errorf("Match(%+v) = %v, want %v", tc.row, got, tc.want)
		}
	}
	if (&RowMatcher{}).Match(&pb.LogRow{CustomerId: 1}) {
		t.Error("a matcher with no coordinates admits nothing")
	}
}
