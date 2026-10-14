package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/jq"
)

// JQMetaVars maps jq underscore-prefixed field names to SQL column names.
// The leading underscore is the disambiguation convention — jq is
// case-sensitive, but real JSONL sometimes carries Pod/.Pod-style keys
// so the underscore is more reliable than casing. See
// docs/plans/ingest-dimensions.md. Exported so compute (compute/
// metavars.go) can expand a row's projected MetaVarColumns back out to
// every alias a jq expression might read.
var JQMetaVars = map[string]string{
	"_namespace":     "namespace",
	"_service":       "service",
	"_kind":          "kind",
	"_level":         "level",
	"_instance":      "instance",
	"_host":          "instance",
	"_ts":            "ts",
	"_deployment":    "deployment",
	"_env":           "env",
	"_version":       "version",
	"_region":        "region",
	"_cluster":       "cluster",
	"_k8s_namespace": "k8s_namespace",
	"_pod":           "pod",
	"_container":     "container",
	"_node":          "node",
	"_trace_id":      "trace_id",
	"_span_name":     "span_name",
	"_span_id":       "span_id",
	"_duration_ns":   "duration_ns",
}

// MetaVarColumns is the distinct, deterministically-ordered set of real
// shard columns backing JQMetaVars (excluding "ts", which every query
// already selects). Appended to the SELECT list whenever a remaining
// jq compute stage might read a bare "._"-prefixed meta-variable for
// output — see Plan's includeMetaVars detection and
// docs/known-issues.md's (fixed) jq meta-variable entry. Order is
// fixed, not map-iteration order, so generated SQL stays stable across
// runs (planner tests pin exact SQL strings).
var MetaVarColumns = []string{
	"namespace", "service", "kind", "level", "instance", "deployment",
	"env", "version", "region", "cluster", "k8s_namespace", "pod",
	"container", "node", "trace_id", "span_name", "span_id", "duration_ns",
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

// jqComparisonToSQL lowers `field op literal` exactly as jq evaluates
// it. jq compares values of different types by a total order,
// null < false < true < numbers < strings < arrays < objects, and a
// missing field is null; so `.n < 100` holds for a row without .n, and
// `.n == 500` doesn't for the string "500". Both sides here are
// literal-typed except the field, so the SQL picks the branch by the
// field's JSON type.
func jqComparisonToSQL(left, right jq.Expr, op string) string {
	field, ok := left.(jq.FieldExpr)
	if !ok {
		return ""
	}
	lit, ok := right.(jq.LiteralExpr)
	if !ok {
		return ""
	}
	if col, isMeta := JQMetaVars[field.Name]; isMeta {
		return jqMetaComparisonToSQL(col, lit.Value, op)
	}

	path := fmt.Sprintf("'$.%s'", escapeSQLString(field.Name))
	var rank int
	var value, sqlLit string
	switch lit.Value.Type {
	case jq.TypeNull:
		rank = jqRankNull
	case jq.TypeBool:
		rank = jqRankBool
		value = fmt.Sprintf("CAST(json_extract(payload, %s) AS BOOLEAN)", path)
		sqlLit = strconv.FormatBool(lit.Value.Bool)
	case jq.TypeNumber:
		rank = jqRankNumber
		value = fmt.Sprintf("CAST(json_extract(payload, %s) AS DOUBLE)", path)
		sqlLit = strconv.FormatFloat(lit.Value.Num, 'g', -1, 64)
	case jq.TypeString:
		rank = jqRankString
		value = fmt.Sprintf("json_extract_string(payload, %s)", path)
		sqlLit = "'" + escapeSQLString(lit.Value.Str) + "'"
	default:
		return ""
	}

	typ := fmt.Sprintf("COALESCE(json_type(payload, %s), 'NULL')", path)
	in := func(ranks []int) string {
		var names []string
		for _, r := range ranks {
			names = append(names, jqRankTypes[r]...)
		}
		return fmt.Sprintf("%s IN ('%s')", typ, strings.Join(names, "', '"))
	}
	same := in([]int{rank})
	if value != "" {
		// Null has one value, so the type test alone decides equality.
		same = fmt.Sprintf("(%s AND %s %s %s)", same, value, "=", sqlLit)
	}
	var lower, higher []int
	for r := range jqRankTypes {
		if r < rank {
			lower = append(lower, r)
		} else if r > rank {
			higher = append(higher, r)
		}
	}
	sameCmp := func(cmp string) string {
		if value == "" {
			// Null only equals null, so < and > within the rank are false.
			if cmp == "<=" || cmp == ">=" {
				return in([]int{rank})
			}
			return ""
		}
		return fmt.Sprintf("(%s AND %s %s %s)", in([]int{rank}), value, cmp, sqlLit)
	}
	or := func(parts ...string) string {
		var keep []string
		for _, p := range parts {
			if p != "" {
				keep = append(keep, p)
			}
		}
		switch len(keep) {
		case 0:
			return "false" // nothing sorts below null
		case 1:
			return keep[0]
		}
		return "(" + strings.Join(keep, " OR ") + ")"
	}
	orIn := func(ranks []int) string {
		if len(ranks) == 0 {
			return ""
		}
		return in(ranks)
	}
	switch op {
	case "=":
		return same
	case "!=":
		return "NOT " + same
	case "<", "<=":
		return or(orIn(lower), sameCmp(op))
	case ">", ">=":
		return or(orIn(higher), sameCmp(op))
	}
	return ""
}

// jq's cross-type order, and the DuckDB json_type names of each rank.
const (
	jqRankNull = iota
	jqRankBool
	jqRankNumber
	jqRankString
	jqRankArray
	jqRankObject
)

var jqRankTypes = [][]string{
	jqRankNull:   {"NULL"},
	jqRankBool:   {"BOOLEAN"},
	jqRankNumber: {"BIGINT", "UBIGINT", "DOUBLE", "HUGEINT"},
	jqRankString: {"VARCHAR"},
	jqRankArray:  {"ARRAY"},
	jqRankObject: {"OBJECT"},
}

// jqMetaComparisonToSQL lowers a meta-variable comparison. jq reads a
// meta-variable as the string compute splices in (the column's text),
// or null when the column is NULL; only string literals lower.
func jqMetaComparisonToSQL(col string, lit jq.Value, op string) string {
	if lit.Type != jq.TypeString {
		return ""
	}
	if col == "ts" || col == "duration_ns" {
		col = fmt.Sprintf("CAST(%s AS VARCHAR)", col)
	}
	cmp := fmt.Sprintf("%s %s '%s'", col, op, escapeSQLString(lit.Str))
	switch op {
	case "!=", "<", "<=":
		// null sorts below every string.
		return fmt.Sprintf("(%s IS NULL OR %s)", col, cmp)
	}
	return cmp
}

// jqProgram is a jq stage split the way weft's jq dialect reads it (see
// docs/plans/planner-ir.md, "jq"): leading select(...)s, then a tail.
//
// A jq stage reading shard rows sees each row's payload. A program made
// only of selects is a row filter: it keeps or drops the row, which
// stays its raw line. Anything with a tail emits jq's output over the
// payload.
type jqProgram struct {
	src string // the expression argument, as written
	// selects is the SQL for each leading select that lowers, stopping
	// at the first that doesn't.
	selects []string
	// allLower is true when every leading select lowered.
	allLower bool
	// tail is what follows the leading selects; nil for a select chain.
	tail  jq.Expr
	raw   bool // -r
	slurp bool // -s: the whole stream is one input, so no select is per-row
}

// analyzeJq parses a jq stage's argv. ok is false for an expression
// that doesn't parse or a flag the planner doesn't know, which leaves
// the stage to run as written.
func analyzeJq(args []string) (*jqProgram, bool) {
	p := &jqProgram{}
	for _, a := range args {
		switch a {
		case "-r", "--raw-output":
			p.raw = true
		case "-c", "--compact-output":
		case "-s", "--slurp":
			p.slurp = true
		default:
			if strings.HasPrefix(a, "-") || p.src != "" {
				return nil, false
			}
			p.src = a
		}
	}
	if p.src == "" {
		return nil, false
	}
	expr, err := jq.Parse(p.src)
	if err != nil {
		return nil, false
	}
	nodes := jqFlattenPipe(expr)
	n := 0
	p.allLower = true
	for n < len(nodes) {
		sel, ok := nodes[n].(jq.SelectExpr)
		if !ok {
			break
		}
		if p.allLower {
			if pred := jqCondToSQL(sel.Cond); pred != "" {
				p.selects = append(p.selects, pred)
			} else {
				p.allLower = false
			}
		}
		n++
	}
	if n < len(nodes) {
		p.tail = nodes[n]
		for _, rest := range nodes[n+1:] {
			p.tail = jq.PipeExpr{Left: p.tail, Right: rest}
		}
	}
	return p, true
}

// selectChain reports whether the program is a row filter.
func (p *jqProgram) selectChain() bool { return p.tail == nil && !p.slurp }

// tryJqStagePushdown pushes a jq stage's leading selects into SQL. A
// select chain whose selects all lower is exact: the stage is a row
// filter SQL computes, so it drops out. Otherwise they're a prefilter
// and the stage still runs, unchanged.
func tryJqStagePushdown(args []string) pushdownResult {
	p, ok := analyzeJq(args)
	if !ok || p.slurp || len(p.selects) == 0 {
		return pushdownResult{}
	}
	return pushdownResult{
		pushed:    true,
		predicate: strings.Join(p.selects, " AND "),
		keep:      !(p.selectChain() && p.allLower),
	}
}

// lowerFirstJqStage returns the SQL projection that computes a jq
// stage reading shard rows, or "" when it must run, over the payload.
// selectsPushed says its leading selects are already in the WHERE
// clause. (A select chain never gets here: it's a row stage.)
func lowerFirstJqStage(stage stageInfo, selectsPushed bool) string {
	p, ok := analyzeJq(stage.args)
	// -r prints strings unquoted, which json_extract doesn't.
	if !ok || p.raw || p.slurp || p.tail == nil || !p.allLower || (len(p.selects) > 0 && !selectsPushed) {
		return ""
	}
	sql, ok := jqValueToSQL(p.tail)
	if !ok {
		return ""
	}
	return wrapJqProjection(sql)
}

// splitJqArgs separates jq's command-line flags from its expression argument.
// Any arg beginning with "-" is a flag; the first non-flag arg is the
// expression. Trailing positional args are ignored (jq only takes one).
func splitJqArgs(args []string) (flags []string, expr string) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			continue
		}
		if expr == "" {
			expr = a
		}
	}
	return flags, expr
}

// jqFlagsHaveRaw reports whether the flag slice includes -r / --raw-output.
func jqFlagsHaveRaw(flags []string) bool {
	for _, f := range flags {
		if f == "-r" || f == "--raw-output" {
			return true
		}
	}
	return false
}

// jqFlattenPipe flattens a left-associative PipeExpr chain into a slice.
func jqFlattenPipe(expr jq.Expr) []jq.Expr {
	if p, ok := expr.(jq.PipeExpr); ok {
		return append(jqFlattenPipe(p.Left), jqFlattenPipe(p.Right)...)
	}
	return []jq.Expr{expr}
}
