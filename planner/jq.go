package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/jq"
)

// JQMetaVars maps the jq variables a program may read ($level, $host) to
// SQL column names. They're variables, not fields, so a payload key of
// the same name (.level) stays distinct from the column, and a program
// can rebind one (`. as $level | ...`); a program that does is opaque to
// the planner and reads its own. Exported so compute can bind a row's
// projected MetaVarColumns to every alias a jq expression might read.
var JQMetaVars = map[string]string{
	"namespace":     "namespace",
	"service":       "service",
	"kind":          "kind",
	"level":         "level",
	"instance":      "instance",
	"host":          "instance",
	"ts":            "ts",
	"deployment":    "deployment",
	"env":           "env",
	"version":       "version",
	"region":        "region",
	"cluster":       "cluster",
	"k8s_namespace": "k8s_namespace",
	"pod":           "pod",
	"container":     "container",
	"node":          "node",
	"trace_id":      "trace_id",
	"span_name":     "span_name",
	"span_id":       "span_id",
	"duration_ns":   "duration_ns",
	"event_id":      "event_id",
	"line_no":       "line_no",
}

// JQNumericMetaColumn reports whether jq reads a meta-variable's column
// as a number; the rest are strings. A NULL duration_ns is null; a
// one-line event is line 0.
func JQNumericMetaColumn(col string) bool { return col == "duration_ns" || col == "line_no" }

// event_id and line_no are in JQMetaVars but not MetaVarColumns: every
// query already selects them as typed columns, and compute reads them
// from there (see rowMetaVars).
//
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
	lit, ok := right.(jq.LiteralExpr)
	if !ok {
		return ""
	}
	if ref, ok := left.(jq.VarRef); ok {
		if col, isMeta := JQMetaVars[ref.Name]; isMeta {
			return jqMetaComparisonToSQL(col, lit.Value, op)
		}
		return ""
	}
	field, ok := left.(jq.FieldExpr)
	if !ok {
		return ""
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

// jqMetaSQL is the SQL value of a meta-variable column as jq reads it: a
// string, or for the numeric columns a number. A one-line event has an
// empty event_id and is line 0.
func jqMetaSQL(col string) string {
	switch col {
	case "event_id":
		return "COALESCE(CAST(event_id AS VARCHAR), '')"
	case "line_no":
		return "COALESCE(line_no, 0)"
	case "ts":
		return "CAST(ts AS VARCHAR)"
	}
	return col
}

// jqMetaComparisonToSQL lowers a meta-variable comparison. jq reads a
// string column as its text and a numeric one as a number, and null when
// the column is NULL. Only a literal of the column's own type lowers: jq
// orders every number below every string, which SQL doesn't.
func jqMetaComparisonToSQL(col string, lit jq.Value, op string) string {
	var rhs string
	switch {
	case JQNumericMetaColumn(col) && lit.Type == jq.TypeNumber:
		rhs = formatAwkNumber(lit.Num)
	case !JQNumericMetaColumn(col) && lit.Type == jq.TypeString:
		rhs = "'" + escapeSQLString(lit.Str) + "'"
	default:
		return ""
	}
	val := jqMetaSQL(col)
	cmp := fmt.Sprintf("%s %s %s", val, op, rhs)
	switch op {
	case "!=", "<", "<=":
		// null sorts below every number and string.
		return fmt.Sprintf("(%s IS NULL OR %s)", val, cmp)
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
	expr  jq.Expr // the whole program
	raw   bool    // -r
	slurp bool    // -s: the whole stream is one input, so no select is per-row
}

// analyzeJq parses a jq stage's argv. The reason is set for an
// expression that doesn't parse or a flag the planner doesn't know,
// which leaves the stage to run as written.
func analyzeJq(args []string) (*jqProgram, reason) {
	p := &jqProgram{}
	for _, a := range args {
		switch a {
		case "-r", "--raw-output":
			p.raw = true
		case "-c", "--compact-output":
		case "-s", "--slurp":
			p.slurp = true
		default:
			if strings.HasPrefix(a, "-") {
				return nil, reason{code: reasonFlag, detail: a}
			}
			if p.src != "" {
				return nil, reason{code: reasonFlag, detail: "a second program argument"}
			}
			p.src = a
		}
	}
	if p.src == "" {
		return nil, because(reasonParse)
	}
	expr, err := jq.Parse(p.src)
	if err != nil {
		return nil, because(reasonParse)
	}
	p.expr = expr
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
	return p, reason{}
}

// selectChain reports whether the program is a row filter.
func (p *jqProgram) selectChain() bool { return p.tail == nil && !p.slurp }

// readsJqMetaVar reports whether a jq program reads a meta-variable
// ($host), or rebinds a name it uses (see JQMetaVars), which it can't
// tell from a read.
func readsJqMetaVar(expr jq.Expr) bool {
	return astContains(expr, func(n any) bool {
		switch e := n.(type) {
		case jq.VarRef:
			_, ok := JQMetaVars[e.Name]
			return ok
		}
		return jqBindsMetaVar(n)
	})
}

// jqBindsMetaVar reports whether a node binds a variable that has a
// meta-variable's name.
func jqBindsMetaVar(n any) bool {
	var name string
	switch e := n.(type) {
	case jq.AsExpr:
		name = e.Var
	case jq.ReduceExpr:
		name = e.Var
	case jq.ForeachExpr:
		name = e.Var
	default:
		return false
	}
	_, ok := JQMetaVars[name]
	return ok
}

// jqFlattenPipe flattens a left-associative PipeExpr chain into a slice.
func jqFlattenPipe(expr jq.Expr) []jq.Expr {
	if p, ok := expr.(jq.PipeExpr); ok {
		return append(jqFlattenPipe(p.Left), jqFlattenPipe(p.Right)...)
	}
	return []jq.Expr{expr}
}
