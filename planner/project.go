package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kevlarcon/warp/pkg/awk"
	"github.com/kevlarcon/warp/pkg/jq"
)

// extractRawExpr examines a compute stage and tries to fold its projection
// into shard-side SQL. Returns:
//
//   - rawExpr: SQL expression for the raw column. Empty when ok=false.
//   - residual: optional replacement compute stage. nil means the original
//     stage is fully covered by rawExpr and can be dropped; non-nil means
//     a partial projection — the caller should swap the original stage for
//     this residual and let the rest run in compute.
//   - ok: true when a projection was extracted (caller should set the SELECT).
func extractRawExpr(stage stageInfo) (rawExpr string, residual *stageInfo, ok bool) {
	switch stage.cmd {
	case "cut":
		expr, complete := extractCutRawExpr(stage.args)
		if expr == "" {
			return "", nil, false
		}
		if !complete {
			return expr, &stage, true
		}
		return expr, nil, true
	case "awk":
		expr, complete := extractAwkRawExpr(stage.args)
		if expr == "" {
			return "", nil, false
		}
		if !complete {
			return expr, &stage, true
		}
		return expr, nil, true
	case "jq":
		return extractJqRawExpr(stage.args)
	}
	return "", nil, false
}

// extractCutRawExpr parses cut flags and returns a SQL expression for the
// selected fields.
func extractCutRawExpr(args []string) (string, bool) {
	fieldsSpec := ""

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-f" && i+1 < len(args):
			i++
			fieldsSpec = args[i]
		case strings.HasPrefix(args[i], "-f"):
			fieldsSpec = args[i][2:]
		case args[i] == "-d" && i+1 < len(args):
			i++ // skip delimiter value
		case strings.HasPrefix(args[i], "-d"):
			// -d with inline value, skip
		}
	}

	if fieldsSpec == "" {
		return "", false
	}

	fields := parseCutFieldSpec(fieldsSpec)
	if len(fields) == 0 {
		return "", false
	}

	return fieldsToRawExpr(fields), true
}

// parseCutFieldSpec parses a cut-style field spec: "1,3", "1-3", "5-", "1,3-5".
func parseCutFieldSpec(spec string) []int {
	var result []int
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			start, err := strconv.Atoi(bounds[0])
			if err != nil {
				continue
			}
			if bounds[1] == "" {
				// Open-ended: "5-" → -5 (negative sentinel)
				result = append(result, -start)
			} else {
				end, err := strconv.Atoi(bounds[1])
				if err != nil {
					continue
				}
				for i := start; i <= end; i++ {
					result = append(result, i)
				}
			}
		} else {
			v, err := strconv.Atoi(part)
			if err == nil {
				result = append(result, v)
			}
		}
	}
	return result
}

// fieldsToRawExpr builds a SQL expression that reconstructs raw from
// selected positional fields.
func fieldsToRawExpr(fields []int) string {
	var exprs []string
	for _, f := range fields {
		if f < 0 {
			start := -f
			exprs = append(exprs, fmt.Sprintf("array_to_string(fields[%d:], ' ')", start))
		} else {
			exprs = append(exprs, fmt.Sprintf("list_extract(fields, %d)", f))
		}
	}
	if len(exprs) == 1 {
		return exprs[0]
	}
	return "concat_ws(' ', " + strings.Join(exprs, ", ") + ")"
}

// extractAwkRawExpr examines an awk program and returns a SQL expression
// if the print can be pushed down.
func extractAwkRawExpr(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}

	// Check for -F flag (custom field separator). If present, the shard's
	// whitespace-split fields won't match awk's splitting, so we can't
	// fully push down.
	program := ""
	hasCustomFS := false
	for i := 0; i < len(args); i++ {
		if args[i] == "-F" || strings.HasPrefix(args[i], "-F") {
			hasCustomFS = true
		} else {
			program = args[i]
		}
	}
	if program == "" {
		return "", false
	}

	prog, err := awk.Parse(program)
	if err != nil || len(prog.Rules) == 0 {
		return "", false
	}

	// Try to convert the entire program's print statements to SQL.
	if !hasCustomFS {
		sqlExpr, _, ok := awkProgramToSQL(prog)
		if ok {
			if sqlExpr == "raw" {
				// Identity operation ({print} or {print $0}) — no change needed.
				return "raw", true
			}
			return sqlExpr, true
		}
	}

	// Fallback: can't push down to SQL, keep in compute.
	return "", false
}

// awkProgramToSQL tries to convert all print statements in an awk program
// to a single SQL expression. Returns the SQL, referenced field indices,
// and whether conversion succeeded.
func awkProgramToSQL(prog *awk.Program) (string, []int, bool) {
	if len(prog.Rules) == 0 || len(prog.Functions) > 0 {
		return "", nil, false
	}

	// We only handle single-rule programs without BEGIN/END blocks.
	if len(prog.Rules) != 1 {
		return "", nil, false
	}

	rule := &prog.Rules[0]
	if rule.Action == nil {
		return "", nil, false
	}

	// Any non-nil pattern is a filter (ExprPattern, RegexPattern) or a
	// scope marker (BEGIN/END). The action alone doesn't capture the
	// filter — claiming the program is "just a projection" here would
	// let the caller drop the stage and silently lose the filter
	// (TestEdge_Awk_FilterAfterLimitFreezeMustSurvive guards against
	// that). The limit-safe field-equality carve-out in planner.go
	// catches the most common case (`tail | awk '$N == "foo"'`)
	// before stages reach this projection extraction; what's left
	// here is the regex / inequality / non-limit-safe shapes, all
	// of which need to keep their pattern through compute.
	if rule.Pattern != nil {
		return "", nil, false
	}

	// Must be a single print statement.
	if len(rule.Action.Stmts) != 1 {
		return "", nil, false
	}
	ps, ok := rule.Action.Stmts[0].(*awk.PrintStmt)
	if !ok {
		return "", nil, false
	}

	return awkPrintToSQL(ps)
}

// awkPrintToSQL converts an awk print statement to a SQL expression.
func awkPrintToSQL(ps *awk.PrintStmt) (string, []int, bool) {
	if len(ps.Args) == 0 {
		return "raw", nil, true
	}

	var parts []string
	var allFields []int
	for _, arg := range ps.Args {
		sql, fields, ok := awkPrintArgToSQL(arg)
		if !ok {
			return "", nil, false
		}
		parts = append(parts, sql)
		allFields = append(allFields, fields...)
	}

	if len(parts) == 1 {
		return parts[0], allFields, true
	}
	return "concat_ws(' ', " + strings.Join(parts, ", ") + ")", allFields, true
}

// awkPrintArgToSQL converts a single awk print argument expression to SQL.
func awkPrintArgToSQL(expr awk.Expr) (string, []int, bool) {
	switch e := expr.(type) {
	case *awk.FieldExpr:
		nl, ok := e.Index.(*awk.NumberLit)
		if !ok {
			return "", nil, false
		}
		n := int(nl.Value)
		if n == 0 {
			return "raw", nil, true
		}
		return fmt.Sprintf("list_extract(fields, %d)", n), []int{n}, true

	case *awk.StringLit:
		return fmt.Sprintf("'%s'", escapeSQLString(e.Value)), nil, true

	case *awk.NumberLit:
		if e.Value == float64(int(e.Value)) {
			return fmt.Sprintf("'%d'", int(e.Value)), nil, true
		}
		return fmt.Sprintf("'%g'", e.Value), nil, true

	case *awk.ConcatExpr:
		leftSQL, leftFields, leftOK := awkPrintArgToSQL(e.Left)
		rightSQL, rightFields, rightOK := awkPrintArgToSQL(e.Right)
		if !leftOK || !rightOK {
			return "", nil, false
		}
		fields := append(leftFields, rightFields...)
		return fmt.Sprintf("concat(%s, %s)", leftSQL, rightSQL), fields, true

	case *awk.IdentExpr:
		if col, ok := metaVars[e.Name]; ok {
			return col, nil, true
		}
		return "", nil, false
	}
	return "", nil, false
}

// extractJqRawExpr folds as much of a jq expression as possible into
// shard-side SQL. Three outcomes:
//
//  1. Whole expression projects: single field, pure-path access (.a.b[0],
//     .a | .b), object construction, or array construction over pushable
//     values. The jq stage drops out entirely (residual=nil).
//  2. A pure-path *prefix* projects but the suffix doesn't (.foo | length,
//     .users | sort_by(.name), .foo | select(.bar==1) — note the inner
//     select is value-relative and must NOT be lifted to a row-level WHERE).
//     We emit the path projection and replace the stage with `jq '<rest>'`.
//  3. Nothing pushable (iter `[]`, recurse `..`, lone builtins, etc.):
//     no projection — caller leaves raw and the original stage alone.
func extractJqRawExpr(args []string) (string, *stageInfo, bool) {
	flags, src := splitJqArgs(args)
	if src == "" {
		return "", nil, false
	}
	expr, err := jq.Parse(src)
	if err != nil {
		return "", nil, false
	}

	// -r projects strings unquoted. Our projection path uses json_extract,
	// which returns JSON-quoted text on string scalars, so folding the
	// whole stage into SELECT would silently drop the raw semantics. Keep
	// a residual jq stage whenever -r is present so the flag is honored.
	raw := jqFlagsHaveRaw(flags)

	// Case 1: whole expression projectable.
	if sql, ok := jqValueToSQL(expr); ok && !raw {
		return wrapJqProjection(sql), nil, true
	}

	// Case 2: pure-path prefix + non-pushable suffix.
	nodes := jqFlattenPipe(expr)
	if len(nodes) < 2 {
		return "", nil, false
	}
	pathLen := 0
	for i, n := range nodes {
		if _, ok := jqPathParts(n); !ok {
			break
		}
		pathLen = i + 1
	}
	if pathLen == 0 || pathLen == len(nodes) {
		// pathLen == len(nodes) shouldn't happen because case 1 would have
		// caught it; pathLen == 0 means there's nothing to push.
		return "", nil, false
	}

	var pathParts []string
	for i := 0; i < pathLen; i++ {
		p, _ := jqPathParts(nodes[i])
		pathParts = append(pathParts, p...)
	}
	rawExpr := fmt.Sprintf("json_extract(payload, '$%s')", strings.Join(pathParts, ""))

	// Round-trip the residual through formatJqExpr. If any node can't be
	// printed back to source, bail out — better to ship the original whole
	// stage than to corrupt it.
	residualSrc := jqRenderPipe(nodes[pathLen:])
	if residualSrc == "" {
		return "", nil, false
	}
	residualArgs := append([]string{}, flags...)
	residualArgs = append(residualArgs, residualSrc)
	return wrapJqProjection(rawExpr), &stageInfo{cmd: "jq", args: residualArgs}, true
}

// wrapJqProjection casts a JSON-returning expression (or metavar column) to
// VARCHAR so the driver can scan it into a string and downstream jq gets the
// JSON-text representation. Without this:
//   - duckdb-go returns json_object / json_array as map/slice — scan fails.
//   - duckdb-go strips quotes from string scalars, breaking jq compatibility
//     (`"u1"` → `u1`) for anything piped into a further jq stage.
// For metavar-mapped columns (e.g., ts is TIMESTAMP) the cast also gives a
// consistent string form instead of driver-native types.
func wrapJqProjection(sql string) string {
	return "CAST(" + sql + " AS VARCHAR)"
}

// jqValueToSQL converts a jq expression into a SQL value expression IF the
// whole expression is fully expressible in SQL (no leftover compute work).
// Returns ok=false otherwise.
func jqValueToSQL(expr jq.Expr) (string, bool) {
	switch e := expr.(type) {
	case jq.FieldExpr:
		if col, isMeta := jqMetaVars[e.Name]; isMeta {
			return col, true
		}
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
// Meta-variables can't appear in a JSONPath, so they fail the check.
func jqPathParts(expr jq.Expr) ([]string, bool) {
	switch e := expr.(type) {
	case jq.FieldExpr:
		if _, isMeta := jqMetaVars[e.Name]; isMeta {
			return nil, false
		}
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

// jqRenderPipe formats a sequence of pipe nodes back into source form.
// Returns "" if any node fails to round-trip (formatJqExpr returns "").
func jqRenderPipe(nodes []jq.Expr) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		s := formatJqExpr(n)
		if s == "" || s == "?" {
			return ""
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " | ")
}

// collectJqKeys recursively collects JSON field names from a jq expression.
func collectJqKeys(expr jq.Expr) []string {
	switch e := expr.(type) {
	case jq.FieldExpr:
		if strings.HasPrefix(e.Name, "_") {
			return nil
		}
		return []string{e.Name}
	case jq.ObjectExpr:
		var keys []string
		for _, pair := range e.Pairs {
			valKeys := collectJqKeys(pair.Value)
			if valKeys == nil {
				return nil
			}
			keys = append(keys, valKeys...)
		}
		return keys
	case jq.PipeExpr:
		rightKeys := collectJqKeys(e.Right)
		if rightKeys != nil {
			return rightKeys
		}
		return collectJqKeys(e.Left)
	case jq.SelectExpr:
		return nil
	case jq.IdentityExpr:
		return nil
	}
	return nil
}

func dedup(nums []int) []int {
	seen := map[int]bool{}
	var result []int
	for _, n := range nums {
		if !seen[n] {
			seen[n] = true
			result = append(result, n)
		}
	}
	return result
}
