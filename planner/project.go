package planner

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kevlarcon/warp/pkg/awk"
	"github.com/kevlarcon/warp/pkg/jq"
)

// extractRawExpr examines a compute stage and returns the SQL expression for
// the raw column plus whether the stage is fully handled by the expression
// (i.e., it can be removed from compute stages).
//
// Returns ("", false) if no projection can be extracted.
func extractRawExpr(stage stageInfo) (string, bool) {
	switch stage.cmd {
	case "cut":
		return extractCutRawExpr(stage.args)
	case "awk":
		return extractAwkRawExpr(stage.args)
	case "jq":
		return extractJqRawExpr(stage.args)
	}
	return "", false
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

	// BEGIN/END blocks run outside the per-row loop — can't be expressed as SQL.
	switch rule.Pattern.(type) {
	case *awk.BeginPattern, *awk.EndPattern:
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

// extractJqRawExpr examines a jq expression for field references.
// For jq, the raw column needs to be the JSON payload text so jq can parse it.
func extractJqRawExpr(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	expr, err := jq.Parse(args[0])
	if err != nil {
		return "", false
	}

	keys := collectJqKeys(expr)
	if len(keys) == 0 {
		return "", false
	}

	// jq needs JSON input — cast payload to text for the raw column.
	return "CAST(payload AS TEXT)", false
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
