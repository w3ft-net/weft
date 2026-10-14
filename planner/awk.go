package planner

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/graybeardcorp/warp/pkg/awk"
)

// awkPushdown holds the result of analyzing an awk program for pushdown.
type awkPushdown struct {
	predicate string // SQL WHERE fragment, or "" if not pushable
	// exact is true when the predicate is exactly the awk expression it
	// came from (or, for a whole program, when SQL computes the stage's
	// output, so the stage is dropped from compute). Otherwise the
	// predicate is a prefilter: true wherever the expression is, so the
	// stage still runs, verbatim, on the rows the shard returns.
	exact      bool
	limit      int  // from NR range, 0 = none
	offset     int  // from NR range, 0 = none
	nrSentinel bool // true if this is an intermediate NR bound, not a final result
}

// AwkMetaVars maps weft meta-variable names to SQL column names. These
// are bare identifiers (LEVEL, NAMESPACE, HOST, ...), NOT $-prefixed —
// $NAME is real awk field-by-variable-index syntax and can't carry a
// string value (see docs/known-issues.md's now-fixed "awk's reserved
// meta-variables ... $NAME syntax can't be fixed" entry for why a
// $-prefixed convention was abandoned). NAMESPACE/SERVICE/KIND address
// the three source-coordinate columns directly (the old $APP, which
// conflated namespace and service, is gone). HOST stays an alias for
// INSTANCE so users coming from OTel/Datadog vocabulary find what they
// expect. See docs/plans/ingest-dimensions.md.
var AwkMetaVars = map[string]string{
	"LEVEL":         "level",
	"NAMESPACE":     "namespace",
	"SERVICE":       "service",
	"KIND":          "kind",
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
	"NODE":          "node",
	"TRACE_ID":      "trace_id",
	"SPAN_NAME":     "span_name",
	"SPAN_ID":       "span_id",
	"DURATION_NS":   "duration_ns",
}

// awkMetaVarPattern matches any AwkMetaVars key used as a whole
// identifier (word-bounded, so "MYHOST" doesn't match "HOST") anywhere
// in an awk program's source text. Built once from AwkMetaVars' keys.
// Used by Plan's includeMetaVars detection (planner.go) to decide
// whether a remaining awk stage might read a meta-variable for output
// — see docs/known-issues.md's now-fixed awk meta-variable entry.
var awkMetaVarPattern = regexp.MustCompile(awkMetaVarPatternSource())

func awkMetaVarPatternSource() string {
	names := make([]string, 0, len(AwkMetaVars))
	for name := range AwkMetaVars {
		names = append(names, name)
	}
	return `\b(` + strings.Join(names, "|") + `)\b`
}

// tryAwkPushdown extracts a SQL predicate from an awk program. It never
// rewrites the program: either the predicate is exact and the stage is
// dropped, or it's a prefilter and the stage runs unchanged in compute.
// Returns nil when nothing can be pushed.
//
// A prefilter P is safe when the stage's output over rows WHERE P equals
// its output over all rows. For a program whose main rules (everything
// but BEGIN/END) all have lowerable patterns, P is the OR of those
// patterns: a record matching none of them runs no action, so it can
// neither print nor change state a later rule or END reads. That
// argument fails, and the program isn't pushed, when:
//   - a rule has no pattern, or a range pattern (it fires on rows that
//     match no pattern of their own);
//   - there's a BEGIN (it can set FS/OFS) or a user function;
//   - NR/FNR appear anywhere other than a pure NR window, since a
//     prefilter changes what they count;
//   - getline reads records the shard has filtered out;
//   - END reads $0, a field or NF, which hold the last record read.
//
// Exact means one main rule, its default print, and no END: that's the
// pattern alone, which SQL computes. See docs/plans/planner-ir.md.
func tryAwkPushdown(program string) *awkPushdown {
	prog, err := awk.Parse(program)
	if err != nil || len(prog.Functions) > 0 {
		return nil
	}

	var main []*awk.Rule
	for i := range prog.Rules {
		r := &prog.Rules[i]
		switch r.Pattern.(type) {
		case *awk.BeginPattern:
			return nil
		case *awk.EndPattern:
			if awkContains(r.Action, isAwkRecordRead) {
				return nil
			}
		case *awk.ExprPattern, *awk.RegexPattern:
			main = append(main, r)
		default:
			return nil
		}
	}
	if len(main) == 0 || awkContains(prog, isAwkGetline) {
		return nil
	}

	exact := len(main) == 1 && len(prog.Rules) == 1 &&
		(main[0].Action == nil || isDefaultPrintAction(main[0].Action))

	// NR window: only as the whole program, where it's a LIMIT/OFFSET.
	if exact {
		if pd := extractAwkPattern(main[0].Pattern); pd != nil && !pd.nrSentinel && pd.limit > 0 {
			return pd
		}
	}
	if awkContains(prog, isAwkRecordCounter) {
		return nil
	}

	var preds []string
	for _, r := range main {
		pd := extractAwkPattern(r.Pattern)
		if pd == nil || pd.nrSentinel || pd.limit > 0 {
			return nil
		}
		exact = exact && pd.exact
		preds = append(preds, pd.predicate)
	}
	if len(preds) == 1 {
		return &awkPushdown{predicate: preds[0], exact: exact}
	}
	for i, p := range preds {
		preds[i] = parenthesizeIfCompound(p)
	}
	return &awkPushdown{predicate: "(" + strings.Join(preds, " OR ") + ")"}
}

// extractAwkPattern lowers one main rule's pattern.
func extractAwkPattern(p awk.Pattern) *awkPushdown {
	switch pat := p.(type) {
	case *awk.ExprPattern:
		return extractAwkExpr(pat.Expr)
	case *awk.RegexPattern:
		// Defensive: current warp parsers represent a bare-regex pattern
		// (awk '/pat/') as an ExprPattern wrapping a RegexLit, which
		// extractAwkRegexLit handles. This branch stays for any warp
		// version/path that still emits a dedicated RegexPattern.
		return &awkPushdown{
			predicate: fmt.Sprintf("regexp_matches(raw, '%s')", escapeSQLString(pat.Pattern)),
			exact:     true,
		}
	}
	return nil
}

func isAwkGetline(n any) bool {
	_, ok := n.(*awk.GetlineExpr)
	return ok
}

func isAwkRecordCounter(n any) bool {
	id, ok := n.(*awk.IdentExpr)
	return ok && (id.Name == "NR" || id.Name == "FNR")
}

// isAwkRecordRead matches what END can read of the last record: $0,
// any field, and NF.
func isAwkRecordRead(n any) bool {
	switch e := n.(type) {
	case *awk.FieldExpr:
		return true
	case *awk.IdentExpr:
		return e.Name == "NF"
	}
	return false
}

// awkContains reports whether match holds for any node in the awk AST
// under root. It walks by reflection so a construct added to warp's awk
// AST can't be silently skipped by a hand-written switch.
func awkContains(root any, match func(any) bool) bool {
	var walk func(v reflect.Value) bool
	walk = func(v reflect.Value) bool {
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if v.IsNil() {
				return false
			}
			if v.CanInterface() && match(v.Interface()) {
				return true
			}
			return walk(v.Elem())
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if walk(v.Field(i)) {
					return true
				}
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				f := v.Index(i)
				if f.Kind() == reflect.Struct && f.CanAddr() {
					f = f.Addr()
				}
				if walk(f) {
					return true
				}
			}
		}
		return false
	}
	return walk(reflect.ValueOf(root))
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
	case *awk.RegexLit:
		return extractAwkRegexLit(e)
	}
	return nil
}

// extractAwkRegexLit handles a bare /re/ used as a truthy expression, which awk
// treats as `$0 ~ /re/` — a positive whole-row regex match. warp's parser
// represents a bare-regex *pattern* (e.g. `awk '/timeout/'`) as an ExprPattern
// wrapping a RegexLit rather than a dedicated RegexPattern, so this is the path
// that fires for the common bare-filter case (and for a /re/ sub-expression
// inside an && / || chain). It mirrors the positive `$0 ~ /re/` MatchExpr case:
// regexp_matches(raw, 'pat') is DuckDB's substring/find form.
func extractAwkRegexLit(e *awk.RegexLit) *awkPushdown {
	return &awkPushdown{
		predicate: fmt.Sprintf("regexp_matches(raw, '%s')", escapeSQLString(e.Pattern)),
		exact:     true,
	}
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

// extractAwkLogical lowers && and ||. An AND whose side doesn't lower
// keeps the side that does, as a prefilter: dropping a conjunct only
// widens the match. An OR needs both sides. There's no NOT over a
// compound for the same reason: negating a prefilter would narrow it.
func extractAwkLogical(e *awk.BinaryExpr, sqlOp string) *awkPushdown {
	left := extractAwkExpr(e.Left)
	right := extractAwkExpr(e.Right)
	if sqlOp == "AND" && (left == nil) != (right == nil) {
		side := left
		if side == nil {
			side = right
		}
		if side.nrSentinel || side.limit > 0 {
			return nil
		}
		return &awkPushdown{predicate: side.predicate}
	}
	if left == nil || right == nil {
		return nil
	}

	// Handle NR range: NR >= X && NR <= Y → LIMIT/OFFSET
	if sqlOp == "AND" && left.nrSentinel && right.nrSentinel {
		return combineNRBounds(left, right)
	}
	if left.nrSentinel || right.nrSentinel || left.limit > 0 || right.limit > 0 {
		// Can't combine NR bound with a regular predicate in AND/OR.
		return nil
	}

	exact := left.exact && right.exact
	if sqlOp == "OR" {
		lp := parenthesizeIfCompound(left.predicate)
		rp := parenthesizeIfCompound(right.predicate)
		return &awkPushdown{predicate: fmt.Sprintf("(%s OR %s)", lp, rp), exact: exact}
	}
	return &awkPushdown{
		predicate: fmt.Sprintf("%s AND %s", left.predicate, right.predicate),
		exact:     exact,
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
		exact:  true,
	}
}

func extractAwkComparison(e *awk.BinaryExpr) *awkPushdown {
	if nr := tryNRComparison(e); nr != nil {
		return nr
	}

	// Put the column on the left, the literal on the right.
	col, lit, op := e.Left, e.Right, e.Op
	if isAwkLiteral(col) {
		col, lit, op = lit, col, flipComparison(op)
	}
	if isAwkLiteral(col) || !isAwkLiteral(lit) {
		return nil // literal-vs-literal, or no literal at all
	}

	sqlCol, kind := awkColumnToSQL(col)
	if sqlCol == "" {
		return nil
	}
	sqlOp := op
	if op == "==" {
		sqlOp = "="
	}

	switch l := lit.(type) {
	case *awk.StringLit:
		// A string constant makes awk compare as strings. raw is never
		// NULL; a NULL meta column reaches awk as an unset variable,
		// which is "", so it needs COALESCE exactly when "" satisfies
		// the comparison.
		if kind != awkColString {
			return nil
		}
		if sqlCol != "raw" && compareStrings("", l.Value, op) {
			sqlCol = fmt.Sprintf("COALESCE(%s, '')", sqlCol)
		}
		return &awkPushdown{predicate: fmt.Sprintf("%s %s '%s'", sqlCol, sqlOp, escapeSQLString(l.Value)), exact: true}
	case *awk.NumberLit:
		// Numeric comparison holds only for a value that looks numeric;
		// the one numeric column is DURATION_NS. Unset is 0.
		if kind != awkColNumber {
			return nil
		}
		if compareNumbers(0, l.Value, op) {
			sqlCol = fmt.Sprintf("COALESCE(%s, 0)", sqlCol)
		}
		return &awkPushdown{predicate: fmt.Sprintf("%s %s %s", sqlCol, sqlOp, formatAwkNumber(l.Value)), exact: true}
	}
	return nil
}

func isAwkLiteral(e awk.Expr) bool {
	switch e.(type) {
	case *awk.StringLit, *awk.NumberLit:
		return true
	}
	return false
}

func flipComparison(op string) string {
	switch op {
	case "<":
		return ">"
	case ">":
		return "<"
	case "<=":
		return ">="
	case ">=":
		return "<="
	}
	return op
}

func compareStrings(a, b, op string) bool {
	switch op {
	case "==":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case ">":
		return a > b
	case "<=":
		return a <= b
	case ">=":
		return a >= b
	}
	return false
}

func compareNumbers(a, b float64, op string) bool {
	switch op {
	case "==":
		return a == b
	case "!=":
		return a != b
	case "<":
		return a < b
	case ">":
		return a > b
	case "<=":
		return a <= b
	case ">=":
		return a >= b
	}
	return false
}

func formatAwkNumber(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%g", v)
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

type awkColKind int

const (
	awkColNone awkColKind = iota
	awkColString
	awkColNumber
)

// awkColumnToSQL maps what an awk comparison can read to a shard column:
// $0 is raw, and a bare reserved name (LEVEL, HOST, ...) is its
// meta-variable column (see AwkMetaVars). Positional fields ($1, $NF)
// never lower: the shard's fields column is split from the line after
// its envelope is stripped, by the schema's separator, so fields[N]
// isn't awk's $N (docs/plans/planner-ir.md, "awk semantics"). TS
// doesn't lower either, since awk sees its text form and SQL a
// TIMESTAMP.
func awkColumnToSQL(expr awk.Expr) (string, awkColKind) {
	switch e := expr.(type) {
	case *awk.FieldExpr:
		if idx, ok := e.Index.(*awk.NumberLit); ok && idx.Value == 0 {
			return "raw", awkColString
		}
	case *awk.IdentExpr:
		switch col := AwkMetaVars[e.Name]; col {
		case "", "ts":
		case "duration_ns":
			return col, awkColNumber
		default:
			return col, awkColString
		}
	}
	return "", awkColNone
}

func extractAwkMatch(e *awk.MatchExpr) *awkPushdown {
	// Get the regex pattern.
	regex, ok := e.Pattern.(*awk.RegexLit)
	if !ok {
		return nil
	}
	pattern := escapeSQLString(regex.Pattern)

	// `$0 ~ /pat/` is a positive regex match; `$0 !~ /pat/` negates it.
	// A non-literal pattern (`$0 ~ "pat"`, a variable) returned nil
	// above and runs in compute, which evaluates it as a dynamic regex.
	//
	// regexp_matches(raw, 'pat') is DuckDB's substring/find form —
	// the equivalent of PostgreSQL's `~`. DuckDB's own `~` operator
	// is whole-string match (SIMILAR TO semantics), which silently
	// returns no rows for typical user patterns.
	predicate := fmt.Sprintf("regexp_matches(raw, '%s')", pattern)
	if e.Negated {
		predicate = "NOT " + predicate
	}

	// Check if matching against $0 (the whole line).
	if fe, ok := e.Expr.(*awk.FieldExpr); ok {
		if nl, ok := fe.Index.(*awk.NumberLit); ok && nl.Value == 0 {
			return &awkPushdown{predicate: predicate, exact: true}
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

// escapeSQLString doubles single quotes for safe embedding in a SQL
// '...' string literal. Regex-pushdown callers (DuckDB's `~` operator)
// use this — the pattern goes in as a plain string literal, with
// regex specials (`^`, `$`, `.`, `*`, `+`, `?`, `(`, `)`, `[`, `]`,
// `{`, `}`, `|`, `\`) preserved so the predicate actually means
// what the user wrote.
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
