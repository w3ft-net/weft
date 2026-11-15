package compute

import (
	"fmt"
	"math"
	"math/big"
	"strconv"

	"github.com/w3ft-net/weft/planner"
	pb "github.com/w3ft-net/weft/proto/weftpb"
)

// keyedAggregator finishes an awk sum, max or min per key. Each shard sends one
// row per key with its value beside it (planner.SumColumn and the columns
// after it); after the merge the rows of one key are adjacent, and this folds
// them into the line awk prints: `key value`.
//
// It checks nothing about the values. The coordinator sent this plan to these
// shards because their field facts say every value is what the aggregate is
// exact over, so the answer is the answer; see docs/plans/field-types.md.
type keyedAggregator struct {
	fn   string
	sink func(*pb.ResultRow) bool

	line string
	seen bool

	// A sum: the total of the shards' exact integer sums.
	sum big.Int
	// A max or min: the best row so far and where it came from.
	best    float64
	bestTxt string
	bestNs  int64
	have    bool

	err error
}

func (a *keyedAggregator) push(row *pb.ResultRow) bool {
	if a.err != nil {
		return false
	}
	if a.seen && row.Raw != a.line {
		if !a.finishKey() {
			return false
		}
	}
	a.line, a.seen = row.Raw, true
	if a.fn == aggKeySum {
		return a.pushSum(row)
	}
	return a.pushExtreme(row)
}

func (a *keyedAggregator) pushSum(row *pb.ResultRow) bool {
	text, ok := row.MetaVars[planner.SumColumn]
	var n big.Int
	if _, parsed := n.SetString(text, 10); !ok || !parsed {
		a.err = fmt.Errorf("compute: aggregate row with a bad %s %q", planner.SumColumn, text)
		return false
	}
	a.sum.Add(&a.sum, &n)
	return true
}

func (a *keyedAggregator) pushExtreme(row *pb.ResultRow) bool {
	best, err := metaFloat(row, planner.BestColumn)
	if err != nil {
		a.err = err
		return false
	}
	ns, err := metaFloat(row, planner.BestNsColumn)
	if err != nil {
		a.err = err
		return false
	}
	better := !a.have ||
		(a.fn == aggKeyMax && best > a.best) || (a.fn == aggKeyMin && best < a.best) ||
		// Equal numbers can be different text ("5" and "5.0"): awk keeps the
		// first row it met, which is the earlier one.
		(best == a.best && int64(ns) < a.bestNs)
	if better {
		a.best, a.bestTxt, a.bestNs, a.have = best, row.MetaVars[planner.BestTxtColumn], int64(ns), true
	}
	return true
}

// finishKey prints the key being folded.
func (a *keyedAggregator) finishKey() bool {
	var text string
	switch a.fn {
	case aggKeySum:
		// A whole number prints as an integer. The sum is exact; awk's float
		// sum is the same wherever it stays under 2^53, and rounds above it.
		text = a.sum.String()
		a.sum.SetInt64(0)
	default:
		have := a.have
		a.have = false
		// The comparison is against an element nothing has set, which is 0:
		// only a number greater than 0 (less than 0 for a min) assigns, and
		// warp's awk does not create the element by reading it. A key whose
		// every value fails the test is not in the output (POSIX awk would
		// print it with an empty value).
		if !have || (a.fn == aggKeyMax && a.best <= 0) || (a.fn == aggKeyMin && a.best >= 0) {
			return true
		}
		text = a.bestTxt
	}
	return a.sink(&pb.ResultRow{Raw: a.line + " " + text})
}

func (a *keyedAggregator) flush() {
	if a.seen && a.err == nil {
		a.finishKey()
	}
}

func (a *keyedAggregator) failure() error { return a.err }

// metaFloat is a number the shard sent beside the key, in a column of the row.
func metaFloat(row *pb.ResultRow, col string) (float64, error) {
	text, ok := row.MetaVars[col]
	if !ok {
		return 0, fmt.Errorf("compute: aggregate row without a %s", col)
	}
	n, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("compute: aggregate row with a bad %s %s", col, strconv.Quote(text))
	}
	return n, nil
}
