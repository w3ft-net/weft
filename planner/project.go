package planner

import (
	"fmt"
	"strings"

	"github.com/graybeardcorp/warp/pkg/awk"
	"github.com/graybeardcorp/warp/pkg/jq"
)

// extractRawExpr reports whether a compute stage is exactly a per-row
// projection SQL can compute, and if so returns the SQL expression for
// the raw column. ok means the stage is fully covered and the caller
// drops it from compute; anything partial stays in compute unchanged.
// jq is decided by lowerFirstJqStage, since what jq reads depends on
// its position.
//
// cut isn't here: it splits raw on tabs (or -d), and the shard's fields
// column is split differently (see awkColumnToSQL), so no cut projection
// is exact.
func extractRawExpr(stage stageInfo) (rawExpr string, ok bool) {
	switch stage.cmd {
	case "awk":
		return extractAwkRawExpr(stage.args)
	}
	return "", false
}

// extractAwkRawExpr handles a program that's a single unpatterned
// `{ print ... }`. Flags (-F, -v OFS=...) change what print emits, so
// any flag disqualifies it.
func extractAwkRawExpr(args []string) (string, bool) {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return "", false
	}
	prog, err := awk.Parse(args[0])
	if err != nil || len(prog.Functions) > 0 || len(prog.Rules) != 1 {
		return "", false
	}
	rule := &prog.Rules[0]
	// A pattern is a filter (or BEGIN/END scope), which a projection
	// would silently drop.
	if rule.Pattern != nil || rule.Action == nil || len(rule.Action.Stmts) != 1 {
		return "", false
	}
	ps, ok := rule.Action.Stmts[0].(*awk.PrintStmt)
	if !ok {
		return "", false
	}
	return awkPrintToSQL(ps)
}

// awkPrintToSQL converts an awk print statement to a SQL expression.
// Arguments join with OFS, which is " " (no flags, no BEGIN).
func awkPrintToSQL(ps *awk.PrintStmt) (string, bool) {
	if len(ps.Args) == 0 {
		return "raw", true
	}
	var parts []string
	for _, arg := range ps.Args {
		sql, ok := awkPrintArgToSQL(arg)
		if !ok {
			return "", false
		}
		parts = append(parts, sql)
	}
	if len(parts) == 1 {
		return parts[0], true
	}
	return "concat_ws(' ', " + strings.Join(parts, ", ") + ")", true
}

// awkPrintArgToSQL converts a single awk print argument to a non-NULL
// SQL string expression with the text awk would print.
func awkPrintArgToSQL(expr awk.Expr) (string, bool) {
	switch e := expr.(type) {
	case *awk.FieldExpr:
		// Only $0. Positional fields don't match the shard's fields
		// column; see awkColumnToSQL.
		if idx, ok := e.Index.(*awk.NumberLit); ok && idx.Value == 0 {
			return "raw", true
		}
		return "", false

	case *awk.StringLit:
		return fmt.Sprintf("'%s'", escapeSQLString(e.Value)), true

	case *awk.NumberLit:
		// awk prints integers as integers and everything else via
		// OFMT, which defaults to %.6g.
		if e.Value == float64(int64(e.Value)) {
			return fmt.Sprintf("'%d'", int64(e.Value)), true
		}
		return fmt.Sprintf("'%.6g'", e.Value), true

	case *awk.ConcatExpr:
		left, lok := awkPrintArgToSQL(e.Left)
		right, rok := awkPrintArgToSQL(e.Right)
		if !lok || !rok {
			return "", false
		}
		return fmt.Sprintf("concat(%s, %s)", left, right), true

	case *awk.IdentExpr:
		// A NULL column reaches awk as an unset variable, which prints
		// as "". TS doesn't lower: awk sees its text form.
		switch col := AwkMetaVars[e.Name]; col {
		case "", "ts":
			return "", false
		default:
			return fmt.Sprintf("COALESCE(CAST(%s AS VARCHAR), '')", col), true
		}
	}
	return "", false
}

// wrapJqProjection renders a JSON-valued SQL expression as the text jq
// prints for it: compact JSON, and `null` for a missing value. The cast
// also keeps the driver from returning json_object/json_array as Go
// maps and stripping the quotes off string scalars.
func wrapJqProjection(sql string) string {
	return "COALESCE(CAST(" + sql + " AS VARCHAR), 'null')"
}

// jqValueToSQL converts a jq expression into a SQL value expression IF the
// whole expression is fully expressible in SQL (no leftover compute work).
// Returns ok=false otherwise.
func jqValueToSQL(expr jq.Expr) (string, bool) {
	switch e := expr.(type) {
	case jq.FieldExpr:
		if col, isMeta := JQMetaVars[e.Name]; isMeta {
			// jq reads a meta-variable as the JSON string compute
			// splices into the payload: the column's text form.
			return fmt.Sprintf("to_json(CAST(%s AS VARCHAR))", col), true
		}
		return fmt.Sprintf("json_extract(payload, '$.%s')", e.Name), true

	case jq.LiteralExpr:
		return jqLiteralToSQL(e.Value)

	case jq.PipeExpr:
		// Pure path chain like .a.b[0] / .a | .b.
		if path, ok := jqPathParts(expr); ok {
			return fmt.Sprintf("json_extract(payload, '$%s')", strings.Join(path, "")), true
		}
		return "", false

	case jq.IndexExpr:
		// Standalone .[n] would need a chained subject — not pushable on its own.
		return "", false

	case jq.ObjectExpr:
		var parts []string
		for _, p := range e.Pairs {
			keyStr, ok := jqObjKeyAsString(p.Key)
			if !ok {
				return "", false
			}
			val, ok := jqValueToSQL(p.Value)
			if !ok {
				return "", false
			}
			parts = append(parts, fmt.Sprintf("'%s', %s", escapeSQLString(keyStr), val))
		}
		return "json_object(" + strings.Join(parts, ", ") + ")", true

	case jq.ArrayExpr:
		elems := jqFlattenComma(e.Elems)
		var parts []string
		for _, elem := range elems {
			val, ok := jqValueToSQL(elem)
			if !ok {
				return "", false
			}
			parts = append(parts, val)
		}
		return "json_array(" + strings.Join(parts, ", ") + ")", true
	}
	return "", false
}

// jqPathParts returns the JSONPath fragments for a pure-path expression
// (FieldExpr / numeric IndexExpr / left-associative pipes thereof).
// Meta-variables can't appear in a JSONPath, so they fail the check.
func jqPathParts(expr jq.Expr) ([]string, bool) {
	switch e := expr.(type) {
	case jq.FieldExpr:
		if _, isMeta := JQMetaVars[e.Name]; isMeta {
			return nil, false
		}
		return []string{"." + e.Name}, true
	case jq.IndexExpr:
		lit, ok := e.Index.(jq.LiteralExpr)
		if !ok || lit.Value.Type != jq.TypeNumber {
			return nil, false
		}
		n := int(lit.Value.Num)
		return []string{fmt.Sprintf("[%d]", n)}, true
	case jq.PipeExpr:
		left, ok := jqPathParts(e.Left)
		if !ok {
			return nil, false
		}
		right, ok := jqPathParts(e.Right)
		if !ok {
			return nil, false
		}
		return append(left, right...), true
	}
	return nil, false
}

// jqLiteralToSQL emits a SQL literal for a jq Value.
func jqLiteralToSQL(v jq.Value) (string, bool) {
	switch v.Type {
	case jq.TypeString:
		return fmt.Sprintf("'%s'", escapeSQLString(v.Str)), true
	case jq.TypeNumber:
		if v.Num == float64(int(v.Num)) {
			return fmt.Sprintf("%d", int(v.Num)), true
		}
		return fmt.Sprintf("%g", v.Num), true
	case jq.TypeBool:
		if v.Bool {
			return "true", true
		}
		return "false", true
	case jq.TypeNull:
		return "NULL", true
	}
	return "", false
}

// jqObjKeyAsString returns the literal string for a jq object key. Both
// `{user: ...}` (parsed as LiteralExpr StrValue) and `{"user": ...}`
// reduce to a string; anything else (computed keys) is unsupported.
func jqObjKeyAsString(k jq.Expr) (string, bool) {
	if lit, ok := k.(jq.LiteralExpr); ok && lit.Value.Type == jq.TypeString {
		return lit.Value.Str, true
	}
	return "", false
}

// jqFlattenComma flattens a CommaExpr tree into a slice. Non-comma exprs
// return as a single-element slice.
func jqFlattenComma(expr jq.Expr) []jq.Expr {
	if c, ok := expr.(jq.CommaExpr); ok {
		return append(jqFlattenComma(c.Left), jqFlattenComma(c.Right)...)
	}
	return []jq.Expr{expr}
}
