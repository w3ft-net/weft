package planner

import (
	"fmt"
	"strings"

	"github.com/kevlarcon/warp/pkg/jq"
)

// jqMetaVars maps jq underscore-prefixed field names to SQL column names.
var jqMetaVars = map[string]string{
	"_app":      "app",
	"_level":    "level",
	"_instance": "instance",
	"_ts":       "ts",
}

// tryJqPushdown attempts to extract a pushable predicate from a jq expression string.
// Returns the SQL WHERE fragment, or "" if not pushable.
func tryJqPushdown(expression string) string {
	expr, err := jq.Parse(expression)
	if err != nil {
		return ""
	}
	return jqExprToSQL(expr)
}

// jqExprToSQL recursively walks a jq expression and extracts pushable predicates.
func jqExprToSQL(expr jq.Expr) string {
	switch e := expr.(type) {
	case jq.SelectExpr:
		return jqCondToSQL(e.Cond)
	case jq.PipeExpr:
		return jqPipeToSQL(e)
	}
	return ""
}

// jqPipeToSQL handles chained jq expressions (a | b).
func jqPipeToSQL(e jq.PipeExpr) string {
	left := jqExprToSQL(e.Left)
	right := jqExprToSQL(e.Right)

	if left != "" && right != "" {
		return left + " AND " + right
	}
	if left != "" {
		return left
	}
	return right
}

// jqCondToSQL converts a jq comparison condition to a SQL fragment.
func jqCondToSQL(cond jq.Expr) string {
	switch e := cond.(type) {
	case jq.EqExpr:
		return jqComparisonToSQL(e.Left, e.Right, "=")
	case jq.NeExpr:
		return jqComparisonToSQL(e.Left, e.Right, "!=")
	case jq.LtExpr:
		return jqComparisonToSQL(e.Left, e.Right, "<")
	case jq.GtExpr:
		return jqComparisonToSQL(e.Left, e.Right, ">")
	case jq.LeExpr:
		return jqComparisonToSQL(e.Left, e.Right, "<=")
	case jq.GeExpr:
		return jqComparisonToSQL(e.Left, e.Right, ">=")
	}
	return ""
}

// jqComparisonToSQL converts a jq comparison (field op literal) to SQL.
func jqComparisonToSQL(left, right jq.Expr, op string) string {
	field, ok := left.(jq.FieldExpr)
	if !ok {
		return ""
	}
	lit, ok := right.(jq.LiteralExpr)
	if !ok {
		return ""
	}

	// Check for meta-variable.
	if col, isMeta := jqMetaVars[field.Name]; isMeta {
		return fmt.Sprintf("%s %s '%s'", col, op, lit.Value.Str)
	}

	// Regular JSON field.
	switch lit.Value.Type {
	case jq.TypeNumber:
		n := lit.Value.Num
		isEquality := op == "=" || op == "!="
		castType := "DOUBLE"
		valStr := fmt.Sprintf("%g", n)
		if isEquality && n == float64(int(n)) {
			castType = "INT"
			valStr = fmt.Sprintf("%d", int(n))
		}
		if !isEquality && n == float64(int(n)) {
			valStr = fmt.Sprintf("%d", int(n))
		}
		return fmt.Sprintf("CAST(json_extract(payload, '$.%s') AS %s) %s %s",
			field.Name, castType, op, valStr)

	case jq.TypeString:
		return fmt.Sprintf("json_extract_string(payload, '$.%s') %s '%s'",
			field.Name, op, escapeSQLString(lit.Value.Str))

	case jq.TypeBool:
		valStr := "false"
		if lit.Value.Bool {
			valStr = "true"
		}
		return fmt.Sprintf("CAST(json_extract(payload, '$.%s') AS BOOL) %s %s",
			field.Name, op, valStr)
	}

	return ""
}

// tryJqStagePushdown analyzes a jq expression for pushdown, supporting partial
// pushdown. If the expression is a pipe chain starting with selects followed by
// non-pushable expressions, the selects become SQL predicates and the rest
// becomes a compute stage.
func tryJqStagePushdown(expression string) pushdownResult {
	expr, err := jq.Parse(expression)
	if err != nil {
		return pushdownResult{}
	}

	// Flatten the pipe chain into a list of expressions.
	nodes := jqFlattenPipe(expr)

	// Walk from left, collecting pushable selects.
	var predicates []string
	splitAt := 0
	for i, node := range nodes {
		if sel, ok := node.(jq.SelectExpr); ok {
			pred := jqCondToSQL(sel.Cond)
			if pred == "" {
				break
			}
			predicates = append(predicates, pred)
			splitAt = i + 1
		} else {
			break
		}
	}

	if len(predicates) == 0 {
		return pushdownResult{}
	}

	pred := strings.Join(predicates, " AND ")

	// If everything was pushable, full pushdown.
	if splitAt == len(nodes) {
		return pushdownResult{pushed: true, predicate: pred}
	}

	// Partial: remaining nodes become compute. If we can't round-trip any of
	// them back to source, bail on partial pushdown — better to keep the
	// whole stage in compute than to ship a corrupted jq command.
	rendered := jqRenderPipe(nodes[splitAt:])
	if rendered == "" {
		return pushdownResult{}
	}
	return pushdownResult{pushed: true, predicate: pred, compute: "jq '" + rendered + "'"}
}

// jqFlattenPipe flattens a left-associative PipeExpr chain into a slice.
func jqFlattenPipe(expr jq.Expr) []jq.Expr {
	if p, ok := expr.(jq.PipeExpr); ok {
		return append(jqFlattenPipe(p.Left), jqFlattenPipe(p.Right)...)
	}
	return []jq.Expr{expr}
}

// formatJqExpr formats a jq AST node back to source-like form. Returns ""
// for unsupported nodes; callers that round-trip residuals must check this
// and fall back rather than emit a corrupted jq command.
func formatJqExpr(expr jq.Expr) string {
	switch e := expr.(type) {
	case jq.SelectExpr:
		inner := formatJqExpr(e.Cond)
		if inner == "" {
			return ""
		}
		return fmt.Sprintf("select(%s)", inner)
	case jq.FieldExpr:
		return "." + e.Name
	case jq.LiteralExpr:
		switch e.Value.Type {
		case jq.TypeNumber:
			n := e.Value.Num
			if n == float64(int(n)) {
				return fmt.Sprintf("%d", int(n))
			}
			return fmt.Sprintf("%g", n)
		case jq.TypeString:
			return fmt.Sprintf("%q", e.Value.Str)
		case jq.TypeBool:
			if e.Value.Bool {
				return "true"
			}
			return "false"
		case jq.TypeNull:
			return "null"
		}
	case jq.EqExpr:
		return formatJqBinOp(e.Left, e.Right, " == ")
	case jq.NeExpr:
		return formatJqBinOp(e.Left, e.Right, " != ")
	case jq.LtExpr:
		return formatJqBinOp(e.Left, e.Right, " < ")
	case jq.GtExpr:
		return formatJqBinOp(e.Left, e.Right, " > ")
	case jq.LeExpr:
		return formatJqBinOp(e.Left, e.Right, " <= ")
	case jq.GeExpr:
		return formatJqBinOp(e.Left, e.Right, " >= ")
	case jq.AndExpr:
		return formatJqBinOp(e.Left, e.Right, " and ")
	case jq.OrExpr:
		return formatJqBinOp(e.Left, e.Right, " or ")
	case jq.NotExpr:
		inner := formatJqExpr(e.Operand)
		if inner == "" {
			return ""
		}
		return inner + " | not"
	case jq.PipeExpr:
		return formatJqBinOp(e.Left, e.Right, " | ")
	case jq.CommaExpr:
		return formatJqBinOp(e.Left, e.Right, ", ")
	case jq.ObjectExpr:
		var pairs []string
		for _, p := range e.Pairs {
			key := formatJqObjKey(p.Key)
			val := formatJqExpr(p.Value)
			if key == "" || val == "" {
				return ""
			}
			pairs = append(pairs, key+": "+val)
		}
		return "{" + strings.Join(pairs, ", ") + "}"
	case jq.ArrayExpr:
		inner := formatJqExpr(e.Elems)
		if inner == "" {
			return ""
		}
		return "[" + inner + "]"
	case jq.IndexExpr:
		idx := formatJqExpr(e.Index)
		if idx == "" {
			return ""
		}
		return "[" + idx + "]"
	case jq.IterExpr:
		// Standalone iter — `.[]` so it can be re-attached after a pipe.
		return ".[]"
	case jq.IdentityExpr:
		return "."
	case jq.RecurseExpr:
		return ".."
	case jq.LengthExpr:
		return "length"
	case jq.KeysExpr:
		return "keys"
	case jq.ValuesExpr:
		return "values"
	case jq.TypeExpr:
		return "type"
	case jq.ToStringExpr:
		return "tostring"
	case jq.ToNumberExpr:
		return "tonumber"
	case jq.AsciiUpExpr:
		return "ascii_upcase"
	case jq.AsciiDownExpr:
		return "ascii_downcase"
	case jq.SortExpr:
		return "sort"
	case jq.SortByExpr:
		key := formatJqExpr(e.Key)
		if key == "" {
			return ""
		}
		return "sort_by(" + key + ")"
	case jq.GroupByExpr:
		key := formatJqExpr(e.Key)
		if key == "" {
			return ""
		}
		return "group_by(" + key + ")"
	case jq.UniqueExpr:
		return "unique"
	case jq.UniqueByExpr:
		key := formatJqExpr(e.Key)
		if key == "" {
			return ""
		}
		return "unique_by(" + key + ")"
	case jq.ReverseExpr:
		return "reverse"
	case jq.MapExpr:
		body := formatJqExpr(e.Body)
		if body == "" {
			return ""
		}
		return "map(" + body + ")"
	case jq.MapValuesExpr:
		body := formatJqExpr(e.Body)
		if body == "" {
			return ""
		}
		return "map_values(" + body + ")"
	}
	return ""
}

// formatJqBinOp renders a binary jq op, propagating empty-string failure
// from either operand.
func formatJqBinOp(l, r jq.Expr, op string) string {
	left := formatJqExpr(l)
	right := formatJqExpr(r)
	if left == "" || right == "" {
		return ""
	}
	return left + op + right
}

// formatJqObjKey formats an object key — bare identifier for string literals.
func formatJqObjKey(expr jq.Expr) string {
	if lit, ok := expr.(jq.LiteralExpr); ok && lit.Value.Type == jq.TypeString {
		return lit.Value.Str
	}
	return formatJqExpr(expr)
}
