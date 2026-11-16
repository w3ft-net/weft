package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/jq"
	"github.com/w3ft-net/weft/backends/logs/dialect"
)

// metaVar is one row meta-variable: its lower-case name, the shard column it
// reads, whether jq reads that column as a number, and whether the shard
// projects the column for compute to hand the stage. HOST stays an alias for
// INSTANCE so users coming from OTel/Datadog vocabulary find what they
// expect; an alias shares its column and is projected once. event_id and
// line_no are not projected: every query already selects them as typed
// columns, and compute reads them from there (see rowMetaVars). ts is not
// either, for the same reason.
type metaVar struct {
	name    string
	column  string
	numeric bool
	project bool
}

// metaVarTable is the one list of row meta-variables. awk spells the names
// upper case (HOST), jq as variables ($host); MetaVarColumns is the
// projected columns in table order, which fixes the generated SQL.
var metaVarTable = []metaVar{
	{name: "namespace", column: "namespace", project: true},
	{name: "service", column: "service", project: true},
	{name: "kind", column: "kind", project: true},
	{name: "level", column: "level", project: true},
	{name: "instance", column: "instance", project: true},
	{name: "host", column: "instance"},
	{name: "ts", column: "ts"},
	{name: "deployment", column: "deployment", project: true},
	{name: "env", column: "env", project: true},
	{name: "version", column: "version", project: true},
	{name: "region", column: "region", project: true},
	{name: "cluster", column: "cluster", project: true},
	{name: "k8s_namespace", column: "k8s_namespace", project: true},
	{name: "pod", column: "pod", project: true},
	{name: "container", column: "container", project: true},
	{name: "node", column: "node", project: true},
	{name: "trace_id", column: "trace_id", project: true},
	{name: "span_name", column: "span_name", project: true},
	{name: "span_id", column: "span_id", project: true},
	{name: "parent_span_id", column: "parent_span_id", project: true},
	{name: "duration_ns", column: "duration_ns", numeric: true, project: true},
	{name: "event_id", column: "event_id"},
	{name: "line_no", column: "line_no", numeric: true},
}

// JQMetaVars maps the jq variables a program may read ($level, $host) to
// SQL column names. They're variables, not fields, so a payload key of
// the same name (.level) stays distinct from the column, and a program
// can rebind one (`. as $level | ...`); a program that does is opaque to
// the planner and reads its own. Exported so compute can bind a row's
// projected MetaVarColumns to every alias a jq expression might read.
var JQMetaVars = metaVarNames(strings.ToLower)

// metaVarNames spells the table's names in a language's case.
func metaVarNames(spell func(string) string) map[string]string {
	m := make(map[string]string, len(metaVarTable))
	for _, v := range metaVarTable {
		m[spell(v.name)] = v.column
	}
	return m
}

// JQNumericMetaColumn reports whether jq reads a meta-variable's column
// as a number; the rest are strings. A NULL duration_ns is null; a
// one-line event is line 0.
func JQNumericMetaColumn(col string) bool {
	for _, v := range metaVarTable {
		if v.column == col && v.numeric {
			return true
		}
	}
	return false
}

// MetaVarColumns is the distinct, deterministically-ordered set of real
// shard columns backing JQMetaVars (excluding "ts", which every query
// already selects). Appended to the SELECT list whenever a remaining
// compute stage might read a meta-variable for output, so generated SQL
// stays stable across runs (planner tests pin exact SQL strings).
var MetaVarColumns = projectedMetaVarColumns()

func projectedMetaVarColumns() []string {
	var cols []string
	seen := map[string]bool{}
	for _, v := range metaVarTable {
		if v.project && !seen[v.column] {
			seen[v.column] = true
			cols = append(cols, v.column)
		}
	}
	return cols
}

// jqPath is a chain of field and index steps from the payload, as
// JSONPath fragments (".a", "[0]"). It's the one place a jq path becomes
// SQL: comparisons, projections and the program's current subject all
// build on it.
type jqPath []string

// jqPathOf returns the path a pure-path expression reads: field steps and
// non-negative literal index steps, bare or piped, and `.` as the empty
// path. A negative index counts from the end in jq, which JSONPath can't
// say, so it isn't a path.
func jqPathOf(expr jq.Expr) (jqPath, bool) {
	switch e := expr.(type) {
	case jq.IdentityExpr:
		return jqPath{}, true
	case jq.FieldExpr:
		return jqPath{"." + e.Name}, true
	case jq.IndexExpr:
		lit, ok := e.Index.(jq.LiteralExpr)
		if !ok || lit.Value.Type != jq.TypeNumber || lit.Value.Num < 0 {
			return nil, false
		}
		return jqPath{fmt.Sprintf("[%d]", int(lit.Value.Num))}, true
	case jq.PipeExpr:
		left, ok := jqPathOf(e.Left)
		if !ok {
			return nil, false
		}
		right, ok := jqPathOf(e.Right)
		if !ok {
			return nil, false
		}
		return left.then(right), true
	}
	return nil, false
}

// then is p followed by q, in a new slice.
func (p jqPath) then(q jqPath) jqPath {
	return append(append(jqPath{}, p...), q...)
}

// jsonPath is the path as JSONPath text.
func (p jqPath) jsonPath() string {
	return "$" + strings.Join(p, "")
}

// guards are the conditions under which jq can read the path without an
// error. A step through the wrong type (.a.b with .a a string, .a[0] with .a
// an object, .a with the payload itself an array or a string) is an error,
// which drops the row; null and missing read as null. The JSON functions read
// all of them as NULL, so each step's parent, the payload included, must have
// the type the step needs.
func (p jqPath) guards() []sqlExpr {
	var guards []sqlExpr
	for i := 0; i < len(p); i++ {
		want := dialect.KindObject
		if strings.HasPrefix(p[i], "[") {
			want = dialect.KindArray
		}
		guards = append(guards, jsonKindIs{path: jqPath(p[:i]).jsonPath(), kinds: []dialect.JSONKind{dialect.KindNull, want}})
	}
	return guards
}

// guarded is cond, true only where the path is readable.
func (p jqPath) guarded(cond sqlExpr) sqlExpr {
	guards := p.guards()
	if len(guards) == 0 {
		return cond
	}
	return groupExpr{and(append(guards, cond)...)}
}

// jqCondToSQL converts a jq comparison condition to a SQL expression, nil for
// one that doesn't lower. The condition reads the value at subject, the path
// the program has navigated to by the time it runs (`.a | select(.b > 1)`
// compares $.a.b).
func jqCondToSQL(subject jqPath, cond jq.Expr) sqlExpr {
	switch e := cond.(type) {
	case jq.EqExpr:
		return jqComparisonToSQL(subject, e.Left, e.Right, dialect.EQ)
	case jq.NeExpr:
		return jqComparisonToSQL(subject, e.Left, e.Right, dialect.NE)
	case jq.LtExpr:
		return jqComparisonToSQL(subject, e.Left, e.Right, dialect.LT)
	case jq.GtExpr:
		return jqComparisonToSQL(subject, e.Left, e.Right, dialect.GT)
	case jq.LeExpr:
		return jqComparisonToSQL(subject, e.Left, e.Right, dialect.LE)
	case jq.GeExpr:
		return jqComparisonToSQL(subject, e.Left, e.Right, dialect.GE)
	}
	return nil
}

// jqTypedLeaf is the simpler form of a select on a top-level key against a
// number, for a shard where that key is a number in every row. jq's order
// puts every number below every string and null below both, so the general
// SQL branches on the value's JSON type and on whether the key is there;
// where it is always a number none of that matters and the comparison is the
// cast alone. base is the predicate jqCondToSQL gave this condition.
func jqTypedLeaf(subject jqPath, cond jq.Expr, base sqlExpr) []typedLeaf {
	if len(subject) != 0 {
		return nil
	}
	var left, right jq.Expr
	var op dialect.CmpOp
	switch e := cond.(type) {
	case jq.EqExpr:
		left, right, op = e.Left, e.Right, dialect.EQ
	case jq.NeExpr:
		left, right, op = e.Left, e.Right, dialect.NE
	case jq.LtExpr:
		left, right, op = e.Left, e.Right, dialect.LT
	case jq.GtExpr:
		left, right, op = e.Left, e.Right, dialect.GT
	case jq.LeExpr:
		left, right, op = e.Left, e.Right, dialect.LE
	case jq.GeExpr:
		left, right, op = e.Left, e.Right, dialect.GE
	default:
		return nil
	}
	field, ok := left.(jq.FieldExpr)
	if !ok {
		return nil
	}
	lit, ok := right.(jq.LiteralExpr)
	if !ok || lit.Value.Type != jq.TypeNumber {
		return nil
	}
	key := jqPath{"." + field.Name}
	return []typedLeaf{{
		locator: "." + field.Name,
		base:    base,
		typed:   cmpExpr{op, tryCastDouble{jsonExtract{key.jsonPath()}}, numLit(typedNumber(lit.Value.Num))},
	}}
}

// jqComparisonToSQL lowers `field op literal` exactly as jq evaluates
// it. jq compares values of different types by a total order,
// null < false < true < numbers < strings < arrays < objects, and a
// missing field is null; so `.n < 100` holds for a row without .n, and
// `.n == 500` doesn't for the string "500". Both sides here are
// literal-typed except the field, so the SQL picks the branch by the
// field's JSON type.
func jqComparisonToSQL(subject jqPath, left, right jq.Expr, op dialect.CmpOp) sqlExpr {
	lit, ok := right.(jq.LiteralExpr)
	if !ok {
		return nil
	}
	if ref, ok := left.(jq.VarRef); ok {
		if col, isMeta := JQMetaVars[ref.Name]; isMeta {
			return jqMetaComparisonToSQL(col, lit.Value, op)
		}
		return nil
	}
	rel, ok := jqPathOf(left)
	if !ok {
		return nil
	}
	path := subject.then(rel)
	if len(path) == 0 || !strings.HasPrefix(path[0], ".") {
		return nil // the payload itself, or an index into it
	}
	cmp := jqPathComparisonToSQL(path, lit.Value, op)
	if cmp == nil {
		return nil
	}
	return path.guarded(cmp)
}

// jqPathComparisonToSQL lowers `path op literal` for a path the caller
// guards (see jqComparisonToSQL).
func jqPathComparisonToSQL(p jqPath, lit jq.Value, op dialect.CmpOp) sqlExpr {
	path := p.jsonPath()
	var rank dialect.JSONKind
	var value, sqlLit sqlExpr
	switch lit.Type {
	case jq.TypeNull:
		rank = dialect.KindNull
	case jq.TypeBool:
		rank = dialect.KindBool
		value = jsonAsBool{path}
		sqlLit = boolLit(lit.Bool)
	case jq.TypeNumber:
		rank = dialect.KindNumber
		value = jsonAsNumber{path}
		sqlLit = numLit(strconv.FormatFloat(lit.Num, 'g', -1, 64))
	case jq.TypeString:
		rank = dialect.KindString
		value = jsonAsString{path}
		sqlLit = strLit(lit.Str)
	default:
		return nil
	}

	in := func(ranks ...dialect.JSONKind) sqlExpr { return jsonKindIs{path: path, kinds: ranks} }
	same := in(rank)
	if value != nil {
		// Null has one value, so the type test alone decides equality.
		same = groupExpr{andExpr{same, cmpExpr{dialect.EQ, value, sqlLit}}}
	}
	var lower, higher []dialect.JSONKind
	for r := dialect.KindNull; r <= dialect.KindObject; r++ {
		if r < rank {
			lower = append(lower, r)
		} else if r > rank {
			higher = append(higher, r)
		}
	}
	sameCmp := func(cmp dialect.CmpOp) sqlExpr {
		if value == nil {
			// Null only equals null, so < and > within the rank are false.
			if cmp == dialect.LE || cmp == dialect.GE {
				return in(rank)
			}
			return nil
		}
		return groupExpr{andExpr{in(rank), cmpExpr{cmp, value, sqlLit}}}
	}
	or := func(parts ...sqlExpr) sqlExpr {
		var keep []sqlExpr
		for _, p := range parts {
			if p != nil {
				keep = append(keep, p)
			}
		}
		switch len(keep) {
		case 0:
			return boolLit(false) // nothing sorts below null
		case 1:
			return keep[0]
		}
		return orExpr{terms: keep}
	}
	orIn := func(ranks []dialect.JSONKind) sqlExpr {
		if len(ranks) == 0 {
			return nil
		}
		return in(ranks...)
	}
	switch op {
	case dialect.EQ:
		return same
	case dialect.NE:
		return notExpr{same}
	case dialect.LT, dialect.LE:
		return or(orIn(lower), sameCmp(op))
	case dialect.GT, dialect.GE:
		return or(orIn(higher), sameCmp(op))
	}
	return nil
}

// jqMetaSQL is the value of a meta-variable column as jq reads it: a
// string, or for the numeric columns a number. A one-line event has an
// empty event_id and is line 0.
func jqMetaSQL(col string) sqlExpr {
	switch col {
	case "event_id":
		return coalesce{castText{colRef("event_id")}, strLit("")}
	case "line_no":
		return coalesce{colRef("line_no"), numLit("0")}
	case "ts":
		return castText{colRef("ts")}
	}
	return colRef(col)
}

// jqMetaComparisonToSQL lowers a meta-variable comparison. jq reads a
// string column as its text and a numeric one as a number, and null when
// the column is NULL. Only a literal of the column's own type lowers: jq
// orders every number below every string, which SQL doesn't.
func jqMetaComparisonToSQL(col string, lit jq.Value, op dialect.CmpOp) sqlExpr {
	var rhs sqlExpr
	switch {
	case JQNumericMetaColumn(col) && lit.Type == jq.TypeNumber:
		rhs = numLit(formatAwkNumber(lit.Num))
	case !JQNumericMetaColumn(col) && lit.Type == jq.TypeString:
		rhs = strLit(lit.Str)
	default:
		return nil
	}
	val := jqMetaSQL(col)
	cmp := cmpExpr{op, val, rhs}
	switch op {
	case dialect.NE, dialect.LT, dialect.LE:
		// null sorts below every number and string.
		return orExpr{terms: []sqlExpr{isNull{val}, cmp}}
	}
	return cmp
}

// jqProgram is a jq stage split the way weft's jq dialect reads it: a head of select(...)s and path steps,
// then a tail.
//
// The head walks the payload. Each path step (`.a`, `[0]`) moves the
// subject, the value the program holds, and each select tests the subject
// and keeps or drops the row (`.a | select(.b > 1)` tests $.a.b). A
// stage reading shard rows sees each row's payload. A program made only
// of selects is a row filter: it keeps or drops the row, which stays its
// raw line. Anything else emits jq's output, which for a head with no
// tail is the subject.
type jqProgram struct {
	src string // the expression argument, as written
	// selects is the SQL for each select in the head that lowers,
	// stopping at the first that doesn't.
	selects []sqlExpr
	// typed are the comparisons in selects with a simpler typed form.
	typed []typedLeaf
	// allLower is true when every select in the head lowered.
	allLower bool
	// subject is the path the head's steps navigate to; empty when it
	// has none.
	subject jqPath
	// tail is what follows the head; nil when the head is the program.
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
head:
	for n < len(nodes) {
		switch node := nodes[n].(type) {
		case jq.SelectExpr:
			if p.allLower {
				if pred := jqCondToSQL(p.subject, node.Cond); pred != nil {
					p.selects = append(p.selects, pred)
					p.typed = append(p.typed, jqTypedLeaf(p.subject, node.Cond, pred)...)
				} else {
					p.allLower = false
				}
			}
		case jq.FieldExpr, jq.IndexExpr:
			// Past a select that didn't lower the subject no longer
			// matters: the stage runs whole.
			step, ok := jqPathOf(node)
			if !ok || !p.allLower {
				break head
			}
			p.subject = p.subject.then(step)
		default:
			break head
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
func (p *jqProgram) selectChain() bool {
	return p.tail == nil && len(p.subject) == 0 && !p.slurp
}

// jqMetaRead is the meta-variables a jq program reads ($host), or rebinds a
// name it uses (see JQMetaVars), which it can't tell from a read.
func jqMetaRead(expr jq.Expr) MetaRead {
	var cols []string
	astContains(expr, func(n any) bool {
		var name string
		switch e := n.(type) {
		case jq.VarRef:
			name = e.Name
		case jq.AsExpr:
			name = e.Var
		case jq.ReduceExpr:
			name = e.Var
		case jq.ForeachExpr:
			name = e.Var
		}
		if col, ok := JQMetaVars[name]; ok {
			cols = append(cols, col)
		}
		return false
	})
	return metaReadOf(cols)
}

// jqFlattenPipe flattens a left-associative PipeExpr chain into a slice.
func jqFlattenPipe(expr jq.Expr) []jq.Expr {
	if p, ok := expr.(jq.PipeExpr); ok {
		return append(jqFlattenPipe(p.Left), jqFlattenPipe(p.Right)...)
	}
	return []jq.Expr{expr}
}
