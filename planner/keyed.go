package planner

import (
	"strconv"

	"github.com/graybeardcorp/warp/pkg/awk"
)

// aggOp is what a keyed aggregate does with the rows of each key.
type aggOp int

const (
	// aggCount is `c[KEY]++`.
	aggCount aggOp = iota
	// aggSum is `s[KEY] += $N`.
	aggSum
	// aggMax is `if ($N > m[KEY]) m[KEY] = $N`, aggMin the same with `<`.
	aggMax
	aggMin
)

// keyedAgg is an awk aggregate per key, recognized from the program (awkKeyed).
// key is the SQL for KEY; field is N of the `$N` a sum, max or min reads.
type keyedAgg struct {
	op    aggOp
	key   string
	field int
}

// facts are the field facts the aggregate is exact under, as the locators the
// coordinator checks on every shard (see QueryPlan.Typed): none for a count.
//
//   - max and min compare `$N` as a number, which SQL's cast does exactly when
//     every row holds a finite decimal there ("$N");
//   - a sum adds `$N` in the order awk meets the rows, which float addition
//     does not do the same way when it is split across shards, so it needs
//     the values to be integers whose partial sums are exact ("$N#int"; the
//     query also checks the sums stay within 2^53, see compute).
func (a keyedAgg) facts() []string {
	if a.op == aggCount {
		return nil
	}
	loc := "$" + strconv.Itoa(a.field)
	if a.op == aggSum {
		return []string{loc, loc + "#int"}
	}
	return []string{loc}
}

// holds is what the field must be known to hold for the aggregate to run in
// the shard, in the words Explain uses.
func (a keyedAgg) holds() string {
	if a.op == aggSum {
		return "integers"
	}
	return "numbers"
}

func (a keyedAgg) render() GroupRender {
	switch a.op {
	case aggSum:
		return RenderKeySum
	case aggMax:
		return RenderKeyMax
	case aggMin:
		return RenderKeyMin
	}
	return RenderKeyCount
}

// awkKeyed recognizes the awk idioms for an aggregate per key,
//
//	{ c[KEY]++ }                          END { for (k in c) print k, c[k] }
//	{ s[KEY] += $N }                      END { for (k in s) print k, s[k] }
//	{ if ($N > m[KEY]) m[KEY] = $N }      END { for (k in m) print k, m[k] }
//
// (and `<` for the min). The shape is matched whole: one unpatterned rule that
// does nothing but aggregate, one END that does nothing but print each key
// and its value. Anything else, a second array, a condition, a different
// print, is not an aggregate this recognizes.
//
// It is the same text as projecting KEY and grouping by it: awk walks
// `for (k in c)` in byte order here (warp sorts the keys), which is the
// order a shard's GROUP BY raw merges in, and each line is `key value`.
// KEY lowers as a print argument does, so a key SQL can't compute leaves
// the stage opaque.
func awkKeyed(prog *awk.Program) (keyedAgg, bool) {
	if len(prog.Functions) > 0 || len(prog.Rules) != 2 {
		return keyedAgg{}, false
	}
	main, end := &prog.Rules[0], &prog.Rules[1]
	if _, ok := end.Pattern.(*awk.EndPattern); !ok || main.Pattern != nil {
		return keyedAgg{}, false
	}
	array, index, field, op, ok := keyedAction(main.Action)
	if !ok || !printsEachKey(end.Action, array) {
		return keyedAgg{}, false
	}
	key, ok := awkPrintArgToSQL(index)
	if !ok {
		return keyedAgg{}, false
	}
	return keyedAgg{op: op, key: key, field: field}, true
}

// keyedAction is whether action is one of the aggregating statements, which
// array it updates and by what key, and the field a sum, max or min reads.
func keyedAction(action *awk.Block) (array string, key awk.Expr, field int, op aggOp, ok bool) {
	if action == nil || len(action.Stmts) != 1 {
		return "", nil, 0, 0, false
	}
	switch st := action.Stmts[0].(type) {
	case *awk.ExprStmt:
		switch e := st.Expr.(type) {
		case *awk.IncDecExpr:
			ref, isRef := e.Operand.(*awk.ArrayRef)
			if e.Op != "++" || !isRef {
				return "", nil, 0, 0, false
			}
			return ref.Name, ref.Index, 0, aggCount, true
		case *awk.AssignExpr:
			ref, isRef := e.Target.(*awk.ArrayRef)
			n, isField := fieldNumber(e.Value)
			if e.Op != "+=" || !isRef || !isField {
				return "", nil, 0, 0, false
			}
			return ref.Name, ref.Index, n, aggSum, true
		}
	case *awk.IfStmt:
		return extremeAction(st)
	}
	return "", nil, 0, 0, false
}

// extremeAction is whether st is `if ($N > m[KEY]) m[KEY] = $N` (or `<`),
// the same field and the same key on both sides.
func extremeAction(st *awk.IfStmt) (array string, key awk.Expr, field int, op aggOp, ok bool) {
	cond, isCmp := st.Cond.(*awk.BinaryExpr)
	if !isCmp || st.Else != nil || st.Then == nil || len(st.Then.Stmts) != 1 {
		return "", nil, 0, 0, false
	}
	switch cond.Op {
	case ">":
		op = aggMax
	case "<":
		op = aggMin
	default:
		return "", nil, 0, 0, false
	}
	n, isField := fieldNumber(cond.Left)
	best, isRef := cond.Right.(*awk.ArrayRef)
	es, isExpr := st.Then.Stmts[0].(*awk.ExprStmt)
	if !isField || !isRef || !isExpr {
		return "", nil, 0, 0, false
	}
	set, isSet := es.Expr.(*awk.AssignExpr)
	if !isSet || set.Op != "=" {
		return "", nil, 0, 0, false
	}
	target, isRef := set.Target.(*awk.ArrayRef)
	m, sameField := fieldNumber(set.Value)
	if !isRef || !sameField || m != n || target.Name != best.Name {
		return "", nil, 0, 0, false
	}
	// The key is read twice; both must be the same SQL.
	a, aok := awkPrintArgToSQL(best.Index)
	b, bok := awkPrintArgToSQL(target.Index)
	if !aok || !bok || a != b {
		return "", nil, 0, 0, false
	}
	return best.Name, best.Index, n, op, true
}

// fieldNumber is N when e is `$N` for a literal N of one or more.
func fieldNumber(e awk.Expr) (int, bool) {
	f, ok := e.(*awk.FieldExpr)
	if !ok {
		return 0, false
	}
	c, ok := awkColumnToSQL(f)
	return c.field, ok && c.field > 0
}

// printsEachKey is whether action is just
// `for (k in array) print k, array[k]`.
func printsEachKey(action *awk.Block, array string) bool {
	if action == nil || len(action.Stmts) != 1 {
		return false
	}
	loop, ok := action.Stmts[0].(*awk.ForInStmt)
	if !ok || loop.Array != array || loop.Body == nil || len(loop.Body.Stmts) != 1 {
		return false
	}
	ps, ok := loop.Body.Stmts[0].(*awk.PrintStmt)
	if !ok || ps.Dest != nil || len(ps.Args) != 2 {
		return false
	}
	k, ok := ps.Args[0].(*awk.IdentExpr)
	if !ok || k.Name != loop.Var {
		return false
	}
	v, ok := ps.Args[1].(*awk.ArrayRef)
	if !ok || v.Name != array {
		return false
	}
	idx, ok := v.Index.(*awk.IdentExpr)
	return ok && idx.Name == loop.Var
}
