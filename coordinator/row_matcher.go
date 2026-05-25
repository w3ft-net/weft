package coordinator

import (
	"strings"

	"github.com/kevlarcon/weft/planner"
	pb "github.com/kevlarcon/weft/proto/weftpb"
)

// RowMatcher filters live rows for a tail subscription.
type RowMatcher struct {
	App      string
	Patterns []planner.GrepPattern
}

// rowComposite rebuilds the "namespace/service/kind" path from a row's
// split coordinates. It mirrors the form the planner extracts from a
// pseudofile path and the index key the ingest handler feeds
// SeenServices, so live-tail allowlist matching (which still works in
// composite terms) lines up. kind is included when present — the
// ingest path defaults it to "main".
func rowComposite(r *pb.LogRow) string {
	if r.Service == "" {
		return r.Namespace
	}
	s := r.Namespace + "/" + r.Service
	if r.Kind != "" {
		s += "/" + r.Kind
	}
	return s
}

// Match returns true if the row passes all filters.
func (m *RowMatcher) Match(row *pb.LogRow) bool {
	if m.App != "" && rowComposite(row) != m.App {
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
