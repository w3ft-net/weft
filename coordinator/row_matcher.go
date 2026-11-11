package coordinator

import (
	"strings"

	"github.com/graybeardcorp/weft/planner"
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
func (m *RowMatcher) Match(row Row) bool {
	key := row.key()
	if !m.Coords.Matches(&key) {
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
