package planner

import "github.com/graybeardcorp/warp/pkg/awk"

// awkTally recognizes the awk idiom for a count per key,
//
//	{ c[KEY]++ } END { for (k in c) print k, c[k] }
//
// and returns the SQL for KEY. The shape is matched whole: one unpatterned
// rule that does nothing but count, one END that does nothing but print each
// key and its count. Anything else, a second array, a condition, a different
// print, is not a tally this recognizes.
//
// It is the same text as projecting KEY and grouping by it: awk walks
// `for (k in c)` in byte order here (warp sorts the keys), which is the
// order a shard's GROUP BY raw merges in, and each line is `key count`.
// KEY lowers as a print argument does, so a key SQL can't compute leaves
// the stage opaque.
func awkTally(prog *awk.Program) (string, bool) {
	if len(prog.Functions) > 0 || len(prog.Rules) != 2 {
		return "", false
	}
	count, end := &prog.Rules[0], &prog.Rules[1]
	if _, ok := end.Pattern.(*awk.EndPattern); !ok || count.Pattern != nil {
		return "", false
	}
	array, key, ok := countsByKey(count.Action)
	if !ok || !printsEachKey(end.Action, array) {
		return "", false
	}
	return awkPrintArgToSQL(key)
}

// countsByKey is whether action is just `array[key]++`, and which.
func countsByKey(action *awk.Block) (array string, key awk.Expr, ok bool) {
	if action == nil || len(action.Stmts) != 1 {
		return "", nil, false
	}
	es, isExpr := action.Stmts[0].(*awk.ExprStmt)
	if !isExpr {
		return "", nil, false
	}
	inc, isInc := es.Expr.(*awk.IncDecExpr)
	if !isInc || inc.Op != "++" {
		return "", nil, false
	}
	ref, isRef := inc.Operand.(*awk.ArrayRef)
	if !isRef {
		return "", nil, false
	}
	return ref.Name, ref.Index, true
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
