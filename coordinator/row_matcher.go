package coordinator

import (
	"strings"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// RowMatcher filters live rows for a tail subscription. The source
// coordinates are matched structurally against the row's columns —
// an empty Namespace means "match every source" (warp.log follow); a
// set Namespace with empty Service/Kind widens to the namespace (or
// namespace+service) level.
type RowMatcher struct {
	Customer                 int64
	Namespace, Service, Kind string
	Patterns                 []planner.GrepPattern
}

// Match returns true if the row passes all filters.
func (m *RowMatcher) Match(row *pb.LogRow) bool {
	if m.Customer != row.CustomerId {
		return false
	}
	if m.Namespace != "" {
		if row.Namespace != m.Namespace {
			return false
		}
		if m.Service != "" && row.Service != m.Service {
			return false
		}
		if m.Kind != "" && row.Kind != m.Kind {
			return false
		}
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
