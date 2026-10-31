package compute

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// valueOrder is `sort -k2 -n` over the lines of an awk aggregate, `key value`,
// as warp's sort orders them (cmdSort): split into fields on blanks, the key is
// field 2 on, compared by the number it leads with, ties broken by the whole
// line. It works on the printed lines, not the keys and values behind them, so a
// key with a blank in it, or none, shifts the fields here exactly as it does for
// the stage this replaces. (Where the key ends, -k2 or -k2,2, cannot matter: a
// number stops at the first blank, and fields have none.)
type valueOrder struct {
	reverse, tieReverse bool
}

// groupValueOrder is the order a GroupSpec asks for, nil for none.
func groupValueOrder(g *pb.GroupSpec) (*valueOrder, error) {
	if g.Order != pb.GroupOrder_GROUP_ORDER_VALUE {
		if g.OrderReverse || g.OrderTieReverse {
			return nil, fmt.Errorf("compute: a sort key without a value order")
		}
		return nil, nil
	}
	return &valueOrder{reverse: g.OrderReverse, tieReverse: g.OrderTieReverse}, nil
}

// keyText is the text of the sort key in line: field 2 on, empty if there is no
// second field.
func (o *valueOrder) keyText(line string) string {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	return strings.Join(fields[1:], " ")
}

// less is whether line a sorts before b.
func (o *valueOrder) less(a, b string) bool {
	c := 0
	switch na, nb := parseLeadingFloat(o.keyText(a)), parseLeadingFloat(o.keyText(b)); {
	case na < nb:
		c = -1
	case na > nb:
		c = 1
	}
	if o.reverse {
		c = -c
	}
	if c == 0 {
		c = strings.Compare(a, b)
		if o.tieReverse {
			c = -c
		}
	}
	return c < 0
}

// parseLeadingFloat is the number sort -n reads at the start of s, as warp reads
// it: the blanks, a sign, digits and at most one dot, 0 if there are none.
// It is warp's, copied: the differential test of the order against warp's own
// sort is what keeps the two together.
func parseLeadingFloat(s string) float64 {
	s = strings.TrimLeft(s, " \t")
	i := 0
	if i < len(s) && (s[i] == '-' || s[i] == '+') {
		i++
	}
	digitsStart := i
	seenDot := false
	for i < len(s) {
		c := s[i]
		if c >= '0' && c <= '9' {
			i++
		} else if c == '.' && !seenDot {
			seenDot = true
			i++
		} else {
			break
		}
	}
	if i == digitsStart {
		return 0
	}
	f, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	return f
}

// sortedAggregator finishes an awk aggregate and then orders the lines it
// printed and keeps the first limit of them (0 keeps all): the `sort -k2 -rn |
// head -n N` that followed it. It holds one line per key until flush.
type sortedAggregator struct {
	inner rowAggregator
	order *valueOrder
	limit int
	out   func(*pb.ResultRow) bool
	lines []string
}

func newSortedAggregator(fn string, order *valueOrder, limit int, sink func(*pb.ResultRow) bool) (*sortedAggregator, error) {
	s := &sortedAggregator{order: order, limit: limit, out: sink}
	inner, err := newAggregator(fn, func(row *pb.ResultRow) bool {
		s.lines = append(s.lines, row.Raw)
		return true
	})
	if err != nil {
		return nil, err
	}
	s.inner = inner
	return s, nil
}

func (s *sortedAggregator) push(row *pb.ResultRow) bool { return s.inner.push(row) }

func (s *sortedAggregator) failure() error { return s.inner.failure() }

func (s *sortedAggregator) flush() {
	s.inner.flush()
	if s.inner.failure() != nil {
		return
	}
	sort.SliceStable(s.lines, func(i, j int) bool { return s.order.less(s.lines[i], s.lines[j]) })
	if s.limit > 0 && len(s.lines) > s.limit {
		s.lines = s.lines[:s.limit]
	}
	for _, l := range s.lines {
		if !s.out(&pb.ResultRow{Raw: l}) {
			return
		}
	}
}
