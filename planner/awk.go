package planner

import (
	"fmt"
	"strings"

	"github.com/kevlarcon/warp/pkg/awk"
)

// awkPushdown holds the result of analyzing an awk program for pushdown.
type awkPushdown struct {
	predicate  string // SQL WHERE fragment, or "" if not pushable
	limit      int    // from NR range, 0 = none
	offset     int    // from NR range, 0 = none
	compute    string // non-empty if the stage partially pushes down, leaving a compute remainder
	nrSentinel bool   // true if this is an intermediate NR bound, not a final result
}

// metaVars maps weft meta-variable names to SQL column names. The
// SERVICE/HOST aliases point at the same columns as APP/INSTANCE so
// users coming from OTel/Datadog vocabulary find what they expect.
// See docs/plans/ingest-dimensions.md.
var metaVars = map[string]string{
	"LEVEL":         "level",
	"APP":           "app",
	"SERVICE":       "app",
	"INSTANCE":      "instance",
	"HOST":          "instance",
	"TS":            "ts",
	"DEPLOYMENT":    "deployment",
	"ENV":           "env",
	"VERSION":       "version",
	"REGION":        "region",
	"CLUSTER":       "cluster",
	"K8S_NAMESPACE": "k8s_namespace",
	"POD":           "pod",
	"CONTAINER":     "container",
}

// tryAwkPushdown attempts to extract a pushable predicate from an awk program string.
// Returns nil if no predicate can be extracted at all.
//
// Three cases:
//  1. Pure filter (pattern only, default action) → full pushdown, no compute
//  2. Pattern + non-default action (no END) → push pattern, compute = action only
//  3. Pattern + END block (multi-rule) → push pattern, compute = entire program
func tryAwkPushdown(program string) *awkPushdown {
	prog, err := awk.Parse(program)
	if err != nil {
		return nil
	}

	if len(prog.Functions) > 0 {
		return nil
	}

	// Find the first rule with a pushable filter pattern.
	var filterRule *awk.Rule
	hasEnd := false
	for i := range prog.Rules {
		r := &prog.Rules[i]
		switch r.Pattern.(type) {
		case *awk.EndPattern:
			hasEnd = true
			continue
		case *awk.BeginPattern:
			return nil
		}
		if filterRule == nil {
			filterRule = r
		}
	}

	if filterRule == nil {
		return nil
	}

	// Extract the predicate from the filter pattern.
	var pd *awkPushdown
	switch pat := filterRule.Pattern.(type) {
	case *awk.ExprPattern:
		pd = extractAwkExpr(pat.Expr)
	case *awk.RegexPattern:
		pd = &awkPushdown{
			predicate: fmt.Sprintf("raw LIKE '%%%s%%'", escapeSQLLike(pat.Pattern)),
		}
	}
	if pd == nil || pd.nrSentinel {
		return nil
	}

	// Pure filter: no action or default print, single rule, no END.
	isDefaultAction := filterRule.Action == nil || isDefaultPrintAction(filterRule.Action)
	if isDefaultAction && len(prog.Rules) == 1 {
		return pd
	}

	// Has END block → push the filter predicate, but the entire program
	// must stay in compute (END needs accumulated state from the action).
	if hasEnd {
		pd.compute = fmt.Sprintf("awk '%s'", program)
		return pd
	}

	// Non-default action, no END → push the pattern, compute is just the action.
	if filterRule.Action != nil && !isDefaultAction {
		pd.compute = fmt.Sprintf("awk '%s'", formatAwkAction(filterRule.Action))
		return pd
	}

	return pd
}

// formatAwkAction formats an awk action block as a string for a compute stage.
func formatAwkAction(block *awk.Block) string {
	var parts []string
	for _, stmt := range block.Stmts {
		parts = append(parts, formatAwkStmt(stmt))
	}
	return "{ " + strings.Join(parts, "; ") + " }"
}

// formatAwkStmt formats a single awk statement back to source form.
func formatAwkStmt(stmt awk.Stmt) string {
	switch s := stmt.(type) {
	case *awk.PrintStmt:
		if len(s.Args) == 0 {
			return "print"
		}
		var args []string
		for _, arg := range s.Args {
			args = append(args, formatAwkExpr(arg))
		}
		return "print " + strings.Join(args, ", ")
	case *awk.PrintfStmt:
		var args []string
		args = append(args, formatAwkExpr(s.Format))
		for _, arg := range s.Args {
			args = append(args, formatAwkExpr(arg))
		}
		return "printf " + strings.Join(args, ", ")
	case *awk.ExprStmt:
		return formatAwkExpr(s.Expr)
	case *awk.AssignStmt:
		return formatAwkExpr(s.Target) + " " + s.Op + " " + formatAwkExpr(s.Value)
	}
	return "?"
}

// formatAwkExpr formats an awk expression back to source form.
func formatAwkExpr(expr awk.Expr) string {
	switch e := expr.(type) {
	case *awk.FieldExpr:
		return "$" + formatAwkExpr(e.Index)
	case *awk.IdentExpr:
		return e.Name
	case *awk.NumberLit:
		if e.Value == float64(int(e.Value)) {
			return fmt.Sprintf("%d", int(e.Value))
		}
		return fmt.Sprintf("%g", e.Value)
	case *awk.StringLit:
		return fmt.Sprintf("%q", e.Value)
	case *awk.IncDecExpr:
		if e.Pre {
			return e.Op + formatAwkExpr(e.Operand)
		}
		return formatAwkExpr(e.Operand) + e.Op
	case *awk.BinaryExpr:
		return formatAwkExpr(e.Left) + " " + e.Op + " " + formatAwkExpr(e.Right)
	}
	return "?"
}

// isDefaultPrintAction checks if a block is just {print $0} or {print}.
func isDefaultPrintAction(block *awk.Block) bool {
	if len(block.Stmts) != 1 {
		return false
	}
	ps, ok := block.Stmts[0].(*awk.PrintStmt)
	if !ok {
		return false
	}
	// print with no args = print $0
	if len(ps.Args) == 0 {
		return true
	}
	// print $0
	if len(ps.Args) == 1 {
		fe, ok := ps.Args[0].(*awk.FieldExpr)
		if !ok {
			return false
		}
		nl, ok := fe.Index.(*awk.NumberLit)
		return ok && nl.Value == 0
	}
	return false
}

// extractAwkExpr recursively extracts a SQL predicate from an awk expression.
func extractAwkExpr(expr awk.Expr) *awkPushdown {
	switch e := expr.(type) {
	case *awk.BinaryExpr:
		return extractAwkBinary(e)
	case *awk.MatchExpr:
		return extractAwkMatch(e)
	}
	return nil
}

func extractAwkBinary(e *awk.BinaryExpr) *awkPushdown {
	switch e.Op {
	case "&&":
		return extractAwkLogical(e, "AND")
	case "||":
		return extractAwkLogical(e, "OR")
	case "==", "!=", "<", ">", "<=", ">=":
		return extractAwkComparison(e)
	}
	return nil
}

func extractAwkLogical(e *awk.BinaryExpr, sqlOp string) *awkPushdown {
	left := extractAwkExpr(e.Left)
	right := extractAwkExpr(e.Right)
	if left == nil || right == nil {
		return nil
	}

	// Handle NR range: NR >= X && NR <= Y → LIMIT/OFFSET
	if sqlOp == "AND" && left.nrSentinel && right.nrSentinel {
		return combineNRBounds(left, right)
	}
	if left.nrSentinel || right.nrSentinel {
		// Can't combine NR bound with a regular predicate in AND/OR.
		return nil
	}

	if sqlOp == "OR" {
		lp := parenthesizeIfCompound(left.predicate)
		rp := parenthesizeIfCompound(right.predicate)
		return &awkPushdown{
			predicate: fmt.Sprintf("(%s OR %s)", lp, rp),
		}
	}
	return &awkPushdown{
		predicate: fmt.Sprintf("%s AND %s", left.predicate, right.predicate),
	}
}

// combineNRBounds merges two NR bound pushdowns into a LIMIT/OFFSET.
func combineNRBounds(a, b *awkPushdown) *awkPushdown {
	// Each NR bound encodes the bound value in limit and the comparison
	// direction in offset: offset=1 means >=, offset=2 means <=.
	lower, upper := 0, 0
	for _, pd := range []*awkPushdown{a, b} {
		switch pd.offset {
		case 1: // >= N
			lower = pd.limit
		case 2: // <= N
			upper = pd.limit
		}
	}
	if lower == 0 || upper == 0 || lower > upper {
		return nil
	}
	return &awkPushdown{
		limit:  upper - lower + 1,
		offset: lower - 1,
	}
}

func extractAwkComparison(e *awk.BinaryExpr) *awkPushdown {
	// Check for NR comparisons first.
	if nr := tryNRComparison(e); nr != nil {
		return nr
	}

	sqlLeft, leftType := awkExprToSQL(e.Left)
	sqlRight, rightType := awkExprToSQL(e.Right)
	if sqlLeft == "" || sqlRight == "" {
		return nil
	}

	// At least one side must be a literal — can't push field-vs-field comparisons.
	if leftType != "number" && leftType != "string" &&
		rightType != "number" && rightType != "string" {
		return nil
	}

	op := e.Op
	// SQL uses = for equality, not ==.
	if op == "==" {
		op = "="
	}

	// If comparing a positional field to a number, cast the field.
	if leftType == "field" && rightType == "number" {
		sqlLeft = fmt.Sprintf("CAST(%s AS DOUBLE)", sqlLeft)
	}
	if rightType == "field" && leftType == "number" {
		sqlRight = fmt.Sprintf("CAST(%s AS DOUBLE)", sqlRight)
	}

	return &awkPushdown{
		predicate: fmt.Sprintf("%s %s %s", sqlLeft, op, sqlRight),
	}
}

// tryNRComparison checks for NR >= N or NR <= N patterns.
// Returns a special awkPushdown encoding the NR bound.
func tryNRComparison(e *awk.BinaryExpr) *awkPushdown {
	ident, ok := e.Left.(*awk.IdentExpr)
	if !ok || ident.Name != "NR" {
		return nil
	}
	num, ok := e.Right.(*awk.NumberLit)
	if !ok {
		return nil
	}
	n := int(num.Value)
	switch e.Op {
	case ">=":
		return &awkPushdown{limit: n, offset: 1, nrSentinel: true} // offset=1 signals >=
	case "<=":
		return &awkPushdown{limit: n, offset: 2, nrSentinel: true} // offset=2 signals <=
	}
	return nil
}

// awkExprToSQL converts an awk expression to a SQL fragment.
// Returns the SQL string and a type hint ("meta", "field", "number", "string").
func awkExprToSQL(expr awk.Expr) (string, string) {
	switch e := expr.(type) {
	case *awk.FieldExpr:
		return awkFieldToSQL(e)
	case *awk.NumberLit:
		// Format as integer if it's a whole number.
		if e.Value == float64(int(e.Value)) {
			return fmt.Sprintf("%d", int(e.Value)), "number"
		}
		return fmt.Sprintf("%g", e.Value), "number"
	case *awk.StringLit:
		return fmt.Sprintf("'%s'", escapeSQLString(e.Value)), "string"
	}
	return "", ""
}

// awkFieldToSQL converts $LEVEL, $APP, $3, etc. to SQL column references.
func awkFieldToSQL(e *awk.FieldExpr) (string, string) {
	switch idx := e.Index.(type) {
	case *awk.IdentExpr:
		if col, ok := metaVars[idx.Name]; ok {
			return col, "meta"
		}
	case *awk.NumberLit:
		n := int(idx.Value)
		if n == 0 {
			return "raw", "field"
		}
		return fmt.Sprintf("list_extract(fields, %d)", n), "field"
	}
	return "", ""
}

func extractAwkMatch(e *awk.MatchExpr) *awkPushdown {
	// Get the regex pattern.
	regex, ok := e.Pattern.(*awk.RegexLit)
	if !ok {
		return nil
	}
	pattern := escapeSQLLike(regex.Pattern)

	// $0 ~ /pat/ or bare match on full record.
	likeOp := "LIKE"
	if e.Negated {
		likeOp = "NOT LIKE"
	}

	// Check if matching against $0 (the whole line).
	if fe, ok := e.Expr.(*awk.FieldExpr); ok {
		if nl, ok := fe.Index.(*awk.NumberLit); ok && nl.Value == 0 {
			return &awkPushdown{
				predicate: fmt.Sprintf("raw %s '%%%s%%'", likeOp, pattern),
			}
		}
	}

	return nil
}

// parenthesizeIfCompound wraps a SQL predicate in parens if it contains AND or OR.
func parenthesizeIfCompound(pred string) string {
	if strings.Contains(pred, " AND ") || strings.Contains(pred, " OR ") {
		return "(" + pred + ")"
	}
	return pred
}

// escapeSQLString escapes single quotes for SQL string literals.
func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// escapeSQLLike escapes characters that are special in SQL LIKE patterns
// and also doubles single quotes so the result is safe to embed in a '...'
// string literal — every caller wraps the output in '%...%' and a stray
// quote in user-supplied input (grep/sed/awk pattern) would otherwise
// terminate the literal and let the rest of the pattern run as SQL.
func escapeSQLLike(s string) string {
	s = strings.ReplaceAll(s, "%", "\\%")
	s = strings.ReplaceAll(s, "_", "\\_")
	s = strings.ReplaceAll(s, "'", "''")
	return s
}
