package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/awk"
	"github.com/graybeardcorp/warp/pkg/jq"
)

// awkProjection reports whether an awk program is exactly a per-row
// projection SQL computes, a single unpatterned `{ print ... }`, and if
// so returns the SQL expression for the raw column.
func awkProjection(prog *awk.Program) (string, bool) {
	if len(prog.Functions) > 0 || len(prog.Rules) != 1 {
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
		// $0 is raw and $N is fields[N], which prints as "" when the
		// line has fewer fields; see awkColumnToSQL.
		c, ok := awkColumnToSQL(e)
		if !ok {
			return "", false
		}
		if c.nullable {
			return fmt.Sprintf("COALESCE(%s, '')", c.sql), true
		}
		return c.sql, true

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
		case "line_no":
			// A one-line event is line 0.
			return "CAST(COALESCE(line_no, 0) AS VARCHAR)", true
		default:
			return fmt.Sprintf("COALESCE(CAST(%s AS VARCHAR), '')", col), true
		}
	}
	return "", false
}

// wrapJqProjection renders a JSON-valued SQL expression as the text jq
// prints for it: compact JSON, and `null` for a missing value. The cast
// also keeps the driver from returning json_object/json_array as Go
// maps and stripping the quotes off string scalars. With -r (raw), a
// string prints as its unescaped text.
func wrapJqProjection(sql string, raw bool) string {
	json := "COALESCE(CAST(" + sql + " AS VARCHAR), 'null')"
	if !raw {
		return json
	}
	return "CASE WHEN json_type(" + sql + ") = 'VARCHAR' THEN json_extract_string(" + sql + ", '$') ELSE " + json + " END"
}

// jqValueToSQL converts a jq expression into a SQL value expression IF the
// whole expression is fully expressible in SQL (no leftover compute work).
// Returns ok=false otherwise.
func jqValueToSQL(expr jq.Expr) (string, bool) {
	switch e := expr.(type) {
	case jq.VarRef:
		col, isMeta := JQMetaVars[e.Name]
		if !isMeta {
			return "", false
		}
		// A string column is its text, a numeric one a number; NULL is null.
		return fmt.Sprintf("to_json(%s)", jqMetaSQL(col)), true

	case jq.FieldExpr:
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
func jqPathParts(expr jq.Expr) ([]string, bool) {
	switch e := expr.(type) {
	case jq.FieldExpr:
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

// cutProjection lowers `cut [-d D] -f LIST` to the SQL expression for
// the line it prints, the way warp's cut does: split raw on each D (a
// tab by default), print the listed fields, in list order and repeats
// included, joined by D, skipping fields past the last. A line without
// D is one field. Any other flag stays opaque.
func cutProjection(args []string) (string, reason) {
	delim, spec := "\t", ""
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case (arg == "-d" || arg == "-f") && i+1 < len(args):
			i++
			if arg == "-d" {
				delim = args[i]
			} else {
				spec = args[i]
			}
		case strings.HasPrefix(arg, "-d") && len(arg) > 2:
			delim = arg[2:]
		case strings.HasPrefix(arg, "-f") && len(arg) > 2:
			spec = arg[2:]
		default:
			return "", because(reasonCutArgs)
		}
	}
	if spec == "" {
		return "", because(reasonCutArgs)
	}
	if delim == "" {
		return "", because(reasonCutDelimiter)
	}
	d := "'" + escapeSQLString(delim) + "'"
	parts := "string_split(raw, " + d + ")"
	var lists []string
	for _, item := range strings.Split(spec, ",") {
		from, to, isRange := strings.Cut(item, "-")
		lo, err := strconv.Atoi(from)
		if err != nil || lo < 1 {
			return "", because(reasonCutFields)
		}
		switch {
		case !isRange:
			lists = append(lists, fmt.Sprintf("[%s[%d]]", parts, lo))
		case to == "":
			lists = append(lists, fmt.Sprintf("%s[%d:]", parts, lo))
		default:
			// N-M with N > M lists nothing, in warp and in the slice.
			hi, err := strconv.Atoi(to)
			if err != nil {
				return "", because(reasonCutFields)
			}
			lists = append(lists, fmt.Sprintf("%s[%d:%d]", parts, lo, hi))
		}
	}
	list := lists[0]
	if len(lists) > 1 {
		list = "flatten([" + strings.Join(lists, ", ") + "])"
	}
	// array_to_string skips the NULLs a field past the last reads as,
	// and returns NULL when every field is past it.
	return "COALESCE(array_to_string(" + list + ", " + d + "), '')", reason{}
}
