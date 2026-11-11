package coordinator

import (
	"strings"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
	"github.com/graybeardcorp/weft/shape"
)

// RowMatcher filters live rows for a tail subscription. Coords is the same
// pinned coordinate set the query's SQL was rendered from (see
// planner.QueryPlan.Coords), so a live row is admitted exactly when the
// historical read would have returned it: the same customer, namespace, env,
// service and kind. An empty set admits nothing.
type RowMatcher struct {
	Coords   shape.Set
	Patterns []planner.GrepPattern
}

// Match returns true if the row passes all filters.
func (m *RowMatcher) Match(row *pb.LogRow) bool {
	if !m.Coords.Matches(row.CustomerId, row.Namespace, row.Env, row.Service, row.Kind) {
		return false
	}
	for _, p := range m.Patterns {
		raw := row.Raw
		pat := p.Pattern
		if p.IgnoreCase {
			raw = strings.ToLower(raw)
			pat = strings.ToLower(pat)
		}
		contains := strings.Contains(raw, pat)
		if p.Invert {
			contains = !contains
		}
		if !contains {
			return false
		}
	}
	return true
}
