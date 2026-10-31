package compute

import (
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/graybeardcorp/weft/planner"
	pb "github.com/graybeardcorp/weft/proto/weftpb"
)

// InexactAggregateError says a keyed aggregate (planner.RenderKeySum, Max and
// Min) could not be shown to equal what awk prints for the same rows. The plan
// chose to compute it on the shards because the shards' field facts said it
// would be exact; a value those facts did not allow, or a sum too large for
// every partial sum to be a whole number a float holds, makes it not so. Nothing
// was printed, so the caller can run the query again without the facts.
type InexactAggregateError struct {
	Reason string
}

func (e *InexactAggregateError) Error() string {
	return "aggregate not exact on the shards: " + e.Reason
}

// maxExactSum is the most the absolute values of a sum's rows may total for
// every partial sum, in any order, to be a whole number a float64 holds. The
// shards add them in floats, so the total is checked a bit under 2^53 to
// leave room for its own rounding.
const maxExactSum = 1 << 52

// keyedAggregator finishes an awk sum, max or min per key. Each shard sends one
// row per key with its value beside it (planner.SumColumn and the columns
// after it); after the merge the rows of one key are adjacent, and this folds
// them into the line awk prints: `key value`.
//
// It prints nothing until every row has been seen, since a value found
// inexact in the last row makes the whole answer unusable.
type keyedAggregator struct {
	fn   string
	sink func(*pb.ResultRow) bool

	line string
	seen bool

	// A sum.
	sum, abs float64
	// A max or min: the best row so far and where it came from.
	best    float64
	bestTxt string
	bestNs  int64
	have    bool

	out []string
	err error
}

func (a *keyedAggregator) push(row *pb.ResultRow) bool {
	if a.err != nil {
		return false
	}
	if a.seen && row.Raw != a.line {
		a.finishKey()
	}
	a.line, a.seen = row.Raw, true
	if bad, err := metaFloat(row, planner.BadColumn); err != nil {
		a.err = err
		return false
	} else if bad != 0 {
		a.err = &InexactAggregateError{Reason: fmt.Sprintf("%.0f value(s) of %q were not numbers the shard could add or compare", bad, row.Raw)}
		return false
	}
	if a.fn == aggKeySum {
		return a.pushSum(row)
	}
	return a.pushExtreme(row)
}

func (a *keyedAggregator) pushSum(row *pb.ResultRow) bool {
	sum, err := metaFloat(row, planner.SumColumn)
	if err != nil {
		a.err = err
		return false
	}
	abs, err := metaFloat(row, planner.AbsSumColumn)
	if err != nil {
		a.err = err
		return false
	}
	a.sum += sum
	a.abs += abs
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
func (a *keyedAggregator) finishKey() {
	var text string
	switch a.fn {
	case aggKeySum:
		if a.abs > maxExactSum {
			a.err = &InexactAggregateError{Reason: fmt.Sprintf("the sum for %q passes 2^52, where float addition may round", a.line)}
			return
		}
		// awk prints a whole number as an integer; every value was one.
		text = strconv.FormatInt(int64(a.sum), 10)
	default:
		// The comparison is against an element nothing has set, which is 0:
		// only a number greater than 0 (less than 0 for a min) assigns, and
		// warp's awk does not create the element by reading it. A key whose
		// every value fails the test is not in the output (POSIX awk would
		// print it with an empty value).
		if (a.fn == aggKeyMax && a.best <= 0) || (a.fn == aggKeyMin && a.best >= 0) {
			a.sum, a.abs, a.have = 0, 0, false
			return
		}
		text = a.bestTxt
	}
	a.out = append(a.out, a.line+" "+text)
	a.sum, a.abs, a.have = 0, 0, false
}

func (a *keyedAggregator) flush() {
	if a.seen && a.err == nil {
		a.finishKey()
	}
	if a.err != nil {
		return
	}
	for _, l := range a.out {
		if !a.sink(&pb.ResultRow{Raw: l}) {
			return
		}
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
		return 0, errors.New("compute: aggregate row with a bad " + col + " " + strconv.Quote(text))
	}
	return n, nil
}
