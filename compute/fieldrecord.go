package compute

import (
	"strings"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// RebuildRecord is the line a stage that reads only the fields spec names
// sees, rebuilt from a row in field form (ResultRow.Fields, ResultRow.
// FieldCount). Each field it names is where it expects it ($1, $3), with a
// placeholder in every position before the last one named so the rest keep
// their places; past that it adds placeholders (and the real final field) only
// when the stage reads NF or $NF, so NF is the line's. Real fields are never
// empty or blank, so the line splits back into the same fields. A line with
// no fields, read through $NF, is sent as it is: there $NF is $0.
//
// The stage sees the same $N, $NF and NF it would on the whole line, which is
// what the planner relies on to send it only these. spec.Positions are
// ascending, as the planner makes them.
func RebuildRecord(spec *pb.FieldSpec, row *pb.ResultRow) string {
	n := int(row.GetFieldCount())
	if n == 0 && spec.Last {
		return row.Raw
	}
	// Positions ascend (planner.FieldSpec), and the row's fields are in the
	// same order: the next position to place is Positions[next].
	m := 0
	if len(spec.Positions) > 0 {
		m = int(spec.Positions[len(spec.Positions)-1])
	}
	last := ""
	if spec.Last {
		last = row.Fields[len(spec.Positions)]
	}

	var b strings.Builder
	b.Grow(2 * max(n, m))
	next := 0
	for i := 1; i <= m && i <= n; i++ {
		b.WriteByte(' ')
		switch {
		case next < len(spec.Positions) && int(spec.Positions[next]) == i:
			b.WriteString(row.Fields[next])
			next++
		case spec.Last && n == i:
			b.WriteString(last)
		default:
			b.WriteByte('_')
		}
	}
	switch {
	case spec.Last:
		for k := n - m - 1; k > 0; k-- {
			b.WriteString(" _")
		}
		if n > m {
			b.WriteByte(' ')
			b.WriteString(last)
		}
	case spec.Nf:
		for k := n - m; k > 0; k-- {
			b.WriteString(" _")
		}
	}
	return strings.TrimPrefix(b.String(), " ")
}

// FieldSpecToProto is the spec as the request carries it; nil for none.
func FieldSpecToProto(s *planner.FieldSpec) *pb.FieldSpec {
	if s == nil {
		return nil
	}
	out := &pb.FieldSpec{Last: s.Last, Nf: s.NF}
	for _, p := range s.Positions {
		out.Positions = append(out.Positions, int32(p))
	}
	return out
}
