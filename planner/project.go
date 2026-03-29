package planner

import (
	"strconv"
	"strings"

	"github.com/kevlarcon/warp/pkg/awk"
	"github.com/kevlarcon/warp/pkg/jq"
)

// extractProjection examines the first compute stage (or the stage that would
// become compute) and extracts a field/key projection if possible.
// Returns the projection and whether the stage is fully handled by the projection
// (i.e., it can be removed from compute stages).
func extractProjection(stage stageInfo) *Project {
	switch stage.cmd {
	case "cut":
		return extractCutProjection(stage.args)
	case "awk":
		return extractAwkProjection(stage.args)
	case "jq":
		return extractJqProjection(stage.args)
	}
	return nil
}

// extractCutProjection parses cut flags to determine which fields are selected.
func extractCutProjection(args []string) *Project {
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
		return nil
	}

	fields := parseCutFieldSpec(fieldsSpec)
	if len(fields) == 0 {
		return nil
	}

	return &Project{
		Fields:   fields,
		Complete: true, // cut is fully handled by the projection
	}
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

// extractAwkProjection examines an awk program for field references in print
// statements. Returns nil if the program references $0 or uses complex expressions.
func extractAwkProjection(args []string) *Project {
	if len(args) == 0 {
		return nil
	}
	prog, err := awk.Parse(args[0])
	if err != nil || len(prog.Rules) == 0 {
		return nil
	}

	// Look at all rules for print statements referencing specific fields.
	var fields []int
	for _, rule := range prog.Rules {
		if rule.Action == nil {
			continue
		}
		refs := extractAwkFieldRefs(rule.Action)
		if refs == nil {
			return nil // can't determine projection (e.g., $0 or complex expr)
		}
		fields = append(fields, refs...)
	}

	if len(fields) == 0 {
		return nil
	}

	// Deduplicate and sort.
	fields = dedup(fields)

	return &Project{
		Fields:   fields,
		Complete: false, // awk still needs to run for formatting
	}
}

// extractAwkFieldRefs walks an awk Block and collects positional field
// references ($1, $2, etc.) from print statements. Returns nil if $0 is
// referenced or if field indices can't be statically determined.
func extractAwkFieldRefs(block *awk.Block) []int {
	var refs []int
	for _, stmt := range block.Stmts {
		ps, ok := stmt.(*awk.PrintStmt)
		if !ok {
			continue
		}
		for _, arg := range ps.Args {
			fieldRefs := collectFieldExprs(arg)
			if fieldRefs == nil {
				return nil // unknown reference
			}
			refs = append(refs, fieldRefs...)
		}
	}
	return refs
}

// collectFieldExprs recursively collects $N references from an awk expression.
// Returns nil if $0 is found or if a field index is non-static.
func collectFieldExprs(expr awk.Expr) []int {
	switch e := expr.(type) {
	case *awk.FieldExpr:
		nl, ok := e.Index.(*awk.NumberLit)
		if !ok {
			return nil // dynamic field reference
		}
		n := int(nl.Value)
		if n == 0 {
			return nil // $0 = whole line
		}
		return []int{n}
	case *awk.ConcatExpr:
		left := collectFieldExprs(e.Left)
		right := collectFieldExprs(e.Right)
		if left == nil || right == nil {
			return nil
		}
		return append(left, right...)
	case *awk.StringLit:
		return []int{} // string literal, no field refs
	case *awk.NumberLit:
		return []int{} // number literal, no field refs
	case *awk.IdentExpr:
		// Variable reference like LEVEL, APP — these are meta-vars, not field refs.
		// We can't project on them since they're separate columns.
		return []int{}
	}
	return nil // unknown expression type
}

// extractJqProjection examines a jq expression for field references in object
// construction or simple field access.
func extractJqProjection(args []string) *Project {
	if len(args) == 0 {
		return nil
	}
	expr, err := jq.Parse(args[0])
	if err != nil {
		return nil
	}

	keys := collectJqKeys(expr)
	if len(keys) == 0 {
		return nil
	}

	return &Project{
		JSONKeys: keys,
		Complete: false, // jq still needs to run for formatting
	}
}

// collectJqKeys recursively collects JSON field names from a jq expression.
func collectJqKeys(expr jq.Expr) []string {
	switch e := expr.(type) {
	case jq.FieldExpr:
		// Skip meta-vars (underscore-prefixed) — those are columns, not JSON keys.
		if strings.HasPrefix(e.Name, "_") {
			return nil
		}
		return []string{e.Name}
	case jq.ObjectExpr:
		var keys []string
		for _, pair := range e.Pairs {
			valKeys := collectJqKeys(pair.Value)
			if valKeys == nil {
				return nil // can't determine
			}
			keys = append(keys, valKeys...)
		}
		return keys
	case jq.PipeExpr:
		// For pipes: collect from the rightmost non-select expression.
		// The left side may be selects (already pushed down).
		rightKeys := collectJqKeys(e.Right)
		if rightKeys != nil {
			return rightKeys
		}
		return collectJqKeys(e.Left)
	case jq.SelectExpr:
		return nil // select is a filter, not a projection
	case jq.IdentityExpr:
		return nil // identity = whole object
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
