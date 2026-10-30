package planner

import (
	"fmt"
	"math"
	"reflect"
	"strconv"
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
	limit      int    // from NR range, 0 = none
	offset     int    // from NR range, 0 = none
	nrSentinel bool   // true if this is an intermediate NR bound, not a final result
	why        reason // why a predicate that isn't exact isn't
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
var AwkMetaVars = metaVarNames(strings.ToUpper)

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
//
// The reason says why nothing pushed, or why the stage still runs; the
// pushdown's own why says why its predicate is only a prefilter.
func tryAwkPushdown(prog *awk.Program) (*awkPushdown, reason) {
	if len(prog.Functions) > 0 {
		return nil, because(reasonAwkFunction)
	}

	var main []*awk.Rule
	for i := range prog.Rules {
		r := &prog.Rules[i]
		switch r.Pattern.(type) {
		case *awk.BeginPattern:
			return nil, because(reasonAwkBegin)
		case *awk.EndPattern:
			if astContains(r.Action, isAwkRecordRead) {
				return nil, because(reasonAwkEndReadsRecord)
			}
		case *awk.ExprPattern, *awk.RegexPattern:
			main = append(main, r)
		default:
			return nil, because(reasonAwkUnpatterned)
		}
	}
	if len(main) == 0 {
		return nil, because(reasonAwkNoMainRule)
	}
	if astContains(prog, isAwkGetline) {
		return nil, because(reasonAwkGetline)
	}

	exact := len(main) == 1 && len(prog.Rules) == 1 &&
		(main[0].Action == nil || isDefaultPrintAction(main[0].Action))

	// NR window: only as the whole program, where it's a LIMIT/OFFSET.
	if exact {
		if pd := extractAwkPattern(main[0].Pattern); pd != nil && !pd.nrSentinel && pd.limit > 0 {
			return pd, reason{}
		}
	}
	if astContains(prog, isAwkRecordCounter) {
		return nil, because(reasonAwkRecordCounter)
	}

	var why reason
	switch {
	case len(main) > 1:
		why = because(reasonAwkRules)
	case len(prog.Rules) > 1:
		why = because(reasonAwkEnd)
	case !exact && !actionLowers(main[0].Action):
		why = because(reasonAwkAction)
	}
	// why is the stage's reason to run: its rules do more than filter.
	// patternWhy is the predicate's reason to be only a prefilter: part
	// of a pattern doesn't lower.
	var patternWhy reason
	var preds []string
	for _, r := range main {
		pd := extractAwkPattern(r.Pattern)
		if pd == nil {
			return nil, awkPatternReason(r.Pattern)
		}
		if pd.nrSentinel || pd.limit > 0 {
			return nil, because(reasonAwkRecordCounter)
		}
		if patternWhy.code == reasonNone && !pd.exact {
			patternWhy = pd.why
		}
		exact = exact && pd.exact
		preds = append(preds, pd.predicate)
	}
	if why.code == reasonNone {
		why = patternWhy
	}
	if len(preds) == 1 {
		return &awkPushdown{predicate: preds[0], exact: exact, why: patternWhy}, why
	}
	for i, p := range preds {
		preds[i] = parenthesizeIfCompound(p)
	}
	return &awkPushdown{predicate: "(" + strings.Join(preds, " OR ") + ")", why: patternWhy}, why
}

// actionLowers is whether a rule's action is something SQL computes, so a
// stage that still runs with it is held back by its pattern, not by it.
func actionLowers(action *awk.Block) bool {
	if action == nil || isDefaultPrintAction(action) {
		return true
	}
	_, ok := awkActionProjection(action)
	return ok
}

// awkPatternReason says why an expression doesn't lower: the common
// causes by name, else that it has no lowering at all.
func awkPatternReason(n any) reason {
	switch {
	case astContains(n, isAwkComputedField):
		return because(reasonAwkComputedField)
	case astContains(n, func(n any) bool {
		id, ok := n.(*awk.IdentExpr)
		return ok && id.Name == "TS"
	}):
		return because(reasonAwkTS)
	}
	return because(reasonAwkPattern)
}

// isAwkComputedField matches a field whose index isn't a constant, and
// isn't $NF, which lowers: $i, $(n+1).
func isAwkComputedField(n any) bool {
	fe, ok := n.(*awk.FieldExpr)
	if !ok {
		return false
	}
	if id, ok := fe.Index.(*awk.IdentExpr); ok && id.Name == "NF" {
		return false
	}
	_, ok = fe.Index.(*awk.NumberLit)
	return !ok
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

// astContains reports whether match holds for any node in the syntax
// tree under root (awk's or jq's). It walks by reflection so a construct
// added to warp's AST can't be silently skipped by a hand-written switch.
func astContains(root any, match func(any) bool) bool {
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
	return match(root) || walk(reflect.ValueOf(root))
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
		missing := e.Left
		if left != nil {
			missing = e.Right
		}
		return &awkPushdown{predicate: side.predicate, why: awkPatternReason(missing)}
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
	why := left.why
	if left.exact {
		why = right.why
	}
	if sqlOp == "OR" {
		lp := parenthesizeIfCompound(left.predicate)
		rp := parenthesizeIfCompound(right.predicate)
		return &awkPushdown{predicate: fmt.Sprintf("(%s OR %s)", lp, rp), exact: exact, why: why}
	}
	return &awkPushdown{
		predicate: fmt.Sprintf("%s AND %s", left.predicate, right.predicate),
		exact:     exact,
		why:       why,
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

	c, ok := awkColumnToSQL(col)
	if !ok {
		return nil
	}
	sqlOp := op
	if op == "==" {
		sqlOp = "="
	}

	if c.number {
		// The numeric columns are DURATION_NS and LINE_NO. Unset is 0.
		l, ok := lit.(*awk.NumberLit)
		if !ok {
			return nil
		}
		sql := c.sql
		if compareNumbers(0, l.Value, op) {
			sql = fmt.Sprintf("COALESCE(%s, 0)", sql)
		}
		return &awkPushdown{predicate: fmt.Sprintf("%s %s %s", sql, sqlOp, formatAwkNumber(l.Value)), exact: true}
	}

	// A string column: awk compares as numbers when both sides look
	// numeric, a string constant included, else as strings. NULL is an
	// unset variable or a missing field: 0 as a number, "" as a string.
	text, n, numeric := awkLiteral(lit)
	if !numeric {
		sql := c.sql
		if c.nullable && compareStrings("", text, op) {
			sql = fmt.Sprintf("COALESCE(%s, '')", sql)
		}
		return &awkPushdown{predicate: fmt.Sprintf("%s %s '%s'", sql, sqlOp, escapeSQLString(text)), exact: true}
	}
	// Which comparison applies depends on whether the value looks
	// numeric to awk (Go's strconv.ParseFloat), which SQL can't decide
	// exactly. A prefilter that holds under either reading is safe;
	// the forms DuckDB doesn't parse as a number (inf, nan, hex) are
	// kept for awk to decide.
	parts := []string{
		fmt.Sprintf("TRY_CAST(%s AS DOUBLE) %s %s", c.sql, sqlOp, strconv.FormatFloat(n, 'g', -1, 64)),
		fmt.Sprintf("%s %s '%s'", c.sql, sqlOp, escapeSQLString(text)),
		fmt.Sprintf("regexp_matches(%s, '^[+-]?(?i:inf|nan|0x)')", c.sql),
	}
	if c.nullable && compareNumbers(0, n, op) {
		parts = append([]string{c.sql + " IS NULL"}, parts...)
	}
	return &awkPushdown{
		predicate: "(" + strings.Join(parts, " OR ") + ")",
		why:       because(reasonAwkStrnum),
	}
}

// awkLiteral is a constant as awk compares it: its text, and its value
// when it looks numeric. A number literal prints as awk formats it.
func awkLiteral(lit awk.Expr) (text string, n float64, numeric bool) {
	switch l := lit.(type) {
	case *awk.NumberLit:
		return formatAwkValue(l.Value), l.Value, true
	case *awk.StringLit:
		t := strings.TrimSpace(l.Value)
		v, err := strconv.ParseFloat(t, 64)
		return l.Value, v, t != "" && err == nil
	}
	return "", 0, false
}

// formatAwkValue is how awk turns a number into text: integers whole,
// everything else with six significant digits.
func formatAwkValue(v float64) string {
	if v == float64(int64(v)) && !math.IsInf(v, 0) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', 6, 64)
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

// awkColumn is what an awk expression reads, as a shard column.
type awkColumn struct {
	sql      string
	nullable bool // NULL reaches awk as unset: 0, or ""
	number   bool // compared as a number (DURATION_NS, LINE_NO)
}

// awkColumnToSQL maps what an awk comparison can read to a shard column:
// $0 is raw, $N (a constant N) is fields[N], which ingest splits the way
// awk does, and a bare reserved name (LEVEL, HOST, ...) is its
// meta-variable column (see AwkMetaVars). $NF is the last field, or $0 on a
// line with none, as awk reads it; length is the byte length of its operand
// (the awk that runs these stages counts bytes). Any other computed field
// ($i, $(n+1)) doesn't lower, and neither does TS, which awk sees as text and
// SQL as a TIMESTAMP.
// awkLastField is $NF: the last of the fields (a negative index counts from
// the end), which is $0 when there are none: NF is 0 on a blank line.
const awkLastField = "COALESCE(list_extract(fields, -1), raw)"

func awkColumnToSQL(expr awk.Expr) (awkColumn, bool) {
	switch e := expr.(type) {
	case *awk.CallExpr:
		if e.Name == "length" && len(e.Args) <= 1 {
			operand := "raw"
			if len(e.Args) == 1 {
				var ok bool
				if operand, ok = awkPrintArgToSQL(e.Args[0]); !ok {
					return awkColumn{}, false
				}
			}
			return awkColumn{sql: "strlen(" + operand + ")", number: true}, true
		}
	case *awk.FieldExpr:
		if id, ok := e.Index.(*awk.IdentExpr); ok && id.Name == "NF" {
			return awkColumn{sql: awkLastField}, true
		}
		idx, ok := e.Index.(*awk.NumberLit)
		switch {
		case !ok || idx.Value != float64(int(idx.Value)) || idx.Value < 0:
		case idx.Value == 0:
			return awkColumn{sql: "raw"}, true
		default:
			return awkColumn{sql: fmt.Sprintf("list_extract(fields, %d)", int(idx.Value)), nullable: true}, true
		}
	case *awk.IdentExpr:
		if e.Name == "length" {
			return awkColumn{sql: "strlen(raw)", number: true}, true
		}
		switch col := AwkMetaVars[e.Name]; col {
		case "", "ts":
		case "duration_ns", "line_no":
			return awkColumn{sql: col, nullable: true, number: true}, true
		case "event_id":
			// A UUID column; awk sees its text.
			return awkColumn{sql: "CAST(event_id AS VARCHAR)", nullable: true}, true
		default:
			return awkColumn{sql: col, nullable: true}, true
		}
	}
	return awkColumn{}, false
}

func extractAwkMatch(e *awk.MatchExpr) *awkPushdown {
	// A non-literal pattern (`$0 ~ "pat"`, a variable) runs in compute,
	// which evaluates it as a dynamic regex.
	regex, ok := e.Pattern.(*awk.RegexLit)
	if !ok {
		return nil
	}
	c, ok := awkColumnToSQL(e.Expr)
	if !ok || c.number {
		return nil
	}
	sql := c.sql
	if c.nullable {
		sql = fmt.Sprintf("COALESCE(%s, '')", sql)
	}
	// regexp_matches is DuckDB's substring/find form — the equivalent
	// of PostgreSQL's `~`. DuckDB's own `~` operator is whole-string
	// match (SIMILAR TO semantics), which silently returns no rows for
	// typical user patterns.
	predicate := fmt.Sprintf("regexp_matches(%s, '%s')", sql, escapeSQLString(regex.Pattern))
	if e.Negated {
		predicate = "NOT " + predicate
	}
	return &awkPushdown{predicate: predicate, exact: true}
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
