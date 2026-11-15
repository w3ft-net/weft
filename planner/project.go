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
	if rule.Pattern != nil || rule.Action == nil {
		return "", false
	}
	return awkActionProjection(rule.Action)
}

// awkActionProjection is the SQL for an action that is a single print.
func awkActionProjection(action *awk.Block) (string, bool) {
	if len(action.Stmts) != 1 {
		return "", false
	}
	ps, ok := action.Stmts[0].(*awk.PrintStmt)
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

	case *awk.CallExpr:
		// length prints as the integer it is.
		c, ok := awkColumnToSQL(e)
		if !ok {
			return "", false
		}
		return "CAST(" + c.sql + " AS VARCHAR)", true

	case *awk.IdentExpr:
		if e.Name == "length" {
			return "CAST(strlen(raw) AS VARCHAR)", true
		}
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

// jqPrintPathSQL is the text jq prints for the value at a payload path. With
// -r a string prints as its unescaped text.
func jqPrintPathSQL(path jqPath, raw bool) string {
	leaf := jqLeafSQL(path)
	if !raw {
		return leaf
	}
	p := path.jsonPath()
	return fmt.Sprintf("CASE WHEN json_type(payload, %s) = 'VARCHAR' THEN json_extract_string(payload, %s) ELSE %s END", p, p, leaf)
}

// jqPrintTailSQL is the text jq prints for a lowered tail whose value is sql
// (from jqValueToSQL). Only -r changes it, for a tail that is a bare string: a
// payload path, or a string literal.
func jqPrintTailSQL(tail jq.Expr, sql string, raw bool) string {
	if !raw {
		return sql
	}
	switch e := tail.(type) {
	case jq.FieldExpr, jq.PipeExpr:
		if path, ok := jqPathOf(e); ok && len(path) > 0 {
			return jqPrintPathSQL(path, true)
		}
	case jq.LiteralExpr:
		if e.Value.Type == jq.TypeString {
			return "'" + escapeSQLString(e.Value.Str) + "'"
		}
	case jq.VarRef:
		// A string meta-variable prints as its text, null as null.
		if col, ok := JQMetaVars[e.Name]; ok && !JQNumericMetaColumn(col) {
			return fmt.Sprintf("COALESCE(CAST(%s AS VARCHAR), 'null')", jqMetaSQL(col))
		}
	}
	return sql
}

// jqValueToSQL converts a jq expression into SQL for the text jq prints for its
// value (compact JSON), IF the whole expression is fully expressible in SQL
// (no leftover compute work). Returns ok=false otherwise. Arrays and objects
// are written out as text rather than built with json_array and json_object,
// so each number in them is written the way jq writes it (jqLeafSQL).
func jqValueToSQL(expr jq.Expr) (string, bool) {
	switch e := expr.(type) {
	case jq.VarRef:
		col, isMeta := JQMetaVars[e.Name]
		if !isMeta {
			return "", false
		}
		// A string column is its text, a numeric one a number; NULL is null.
		return fmt.Sprintf("COALESCE(CAST(to_json(%s) AS VARCHAR), 'null')", jqMetaSQL(col)), true

	case jq.FieldExpr:
		return jqLeafSQL(jqPath{"." + e.Name}), true

	case jq.LiteralExpr:
		return jqLiteralToSQL(e.Value)

	case jq.PipeExpr:
		// Pure path chain like .a.b[0] / .a | .b.
		if path, ok := jqPathOf(expr); ok && len(path) > 0 {
			return jqLeafSQL(path), true
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
			parts = append(parts, fmt.Sprintf("'%s:' || %s", escapeSQLString(jqStringJSON(keyStr)), val))
		}
		if len(parts) == 0 {
			return "'{}'", true
		}
		return "'{' || " + strings.Join(parts, " || ',' || ") + " || '}'", true

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
		return "'[' || " + strings.Join(parts, " || ',' || ") + " || ']'", true
	}
	return "", false
}

// jqValueGuards are the conditions under which every payload path a
// jqValueToSQL expression reads is readable: where one isn't, jq errors and
// the row prints nothing. Each condition once.
func jqValueGuards(expr jq.Expr) []string {
	var guards []string
	seen := map[string]bool{}
	var walk func(e jq.Expr)
	walk = func(e jq.Expr) {
		switch e := e.(type) {
		case jq.FieldExpr, jq.PipeExpr:
			if path, ok := jqPathOf(e); ok {
				for _, g := range path.guards() {
					if !seen[g] {
						seen[g] = true
						guards = append(guards, g)
					}
				}
			}
		case jq.ObjectExpr:
			for _, p := range e.Pairs {
				walk(p.Value)
			}
		case jq.ArrayExpr:
			for _, elem := range jqFlattenComma(e.Elems) {
				walk(elem)
			}
		}
	}
	walk(expr)
	return guards
}

// jqLiteralToSQL emits the SQL text jq prints for a literal value.
func jqLiteralToSQL(v jq.Value) (string, bool) {
	switch v.Type {
	case jq.TypeString:
		return "'" + escapeSQLString(jqStringJSON(v.Str)) + "'", true
	case jq.TypeNumber:
		return "'" + jqNumberText(v.Num) + "'", true
	case jq.TypeBool:
		if v.Bool {
			return "'true'", true
		}
		return "'false'", true
	case jq.TypeNull:
		return "'null'", true
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
