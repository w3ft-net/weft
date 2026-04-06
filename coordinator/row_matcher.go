package coordinator

import (
	"strings"

	pb "github.com/kevlarcon/weft/proto/weftpb"
	"github.com/kevlarcon/weft/planner"
)

// RowMatcher filters live rows for a tail subscription.
type RowMatcher struct {
	App      string
	Patterns []planner.GrepPattern
}

// Match returns true if the row passes all filters.
func (m *RowMatcher) Match(row *pb.LogRow) bool {
	if m.App != "" && row.App != m.App {
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
