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

	// Partial: remaining nodes become compute.
	var remainParts []string
	for _, node := range nodes[splitAt:] {
		remainParts = append(remainParts, formatJqExpr(node))
	}
	compute := "jq '" + strings.Join(remainParts, " | ") + "'"

	return pushdownResult{pushed: true, predicate: pred, compute: compute}
}

// jqFlattenPipe flattens a left-associative PipeExpr chain into a slice.
func jqFlattenPipe(expr jq.Expr) []jq.Expr {
	if p, ok := expr.(jq.PipeExpr); ok {
		return append(jqFlattenPipe(p.Left), jqFlattenPipe(p.Right)...)
	}
	return []jq.Expr{expr}
}

// formatJqExpr formats a jq AST node back to source-like string.
func formatJqExpr(expr jq.Expr) string {
	switch e := expr.(type) {
	case jq.SelectExpr:
		return fmt.Sprintf("select(%s)", formatJqExpr(e.Cond))
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
		return formatJqExpr(e.Left) + " == " + formatJqExpr(e.Right)
	case jq.NeExpr:
		return formatJqExpr(e.Left) + " != " + formatJqExpr(e.Right)
	case jq.LtExpr:
		return formatJqExpr(e.Left) + " < " + formatJqExpr(e.Right)
	case jq.GtExpr:
		return formatJqExpr(e.Left) + " > " + formatJqExpr(e.Right)
	case jq.LeExpr:
		return formatJqExpr(e.Left) + " <= " + formatJqExpr(e.Right)
	case jq.GeExpr:
		return formatJqExpr(e.Left) + " >= " + formatJqExpr(e.Right)
	case jq.PipeExpr:
		return formatJqExpr(e.Left) + " | " + formatJqExpr(e.Right)
	case jq.ObjectExpr:
		var pairs []string
		for _, p := range e.Pairs {
			key := formatJqObjKey(p.Key)
			pairs = append(pairs, key+": "+formatJqExpr(p.Value))
		}
		return "{" + strings.Join(pairs, ", ") + "}"
	case jq.IdentityExpr:
		return "."
	}
	return "?"
}

// formatJqObjKey formats an object key — bare identifier for string literals.
func formatJqObjKey(expr jq.Expr) string {
	if lit, ok := expr.(jq.LiteralExpr); ok && lit.Value.Type == jq.TypeString {
		return lit.Value.Str
	}
	return formatJqExpr(expr)
}
