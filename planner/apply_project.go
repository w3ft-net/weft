package planner

import (
	"fmt"
	"strings"
)

// ApplyProjection modifies a SQL query to narrow the SELECT clause based on
// the projection. For field projections, it reconstructs `raw` from selected
// fields. For JSON key projections, it extracts only the needed keys.
//
// Returns the modified SQL. If proj is nil, returns the original SQL unchanged.
func ApplyProjection(sql string, proj *Project) string {
	if proj == nil {
		return sql
	}

	if proj.RawExpr != "" {
		return applyRawExprProjection(sql, proj)
	}
	if len(proj.Fields) > 0 {
		return applyFieldProjection(sql, proj.Fields)
	}
	if len(proj.JSONKeys) > 0 {
		return applyJSONProjection(sql, proj.JSONKeys)
	}
	return sql
}

// applyRawExprProjection rewrites SELECT * using an explicit SQL expression
// for the raw column (e.g. from awk print with concatenation).
func applyRawExprProjection(sql string, proj *Project) string {
	selectClause := fmt.Sprintf(
		"SELECT line_num, %s as raw, fields, ts, level, app, instance, customer, payload, host_group",
		proj.RawExpr)
	return strings.Replace(sql, "SELECT *", selectClause, 1)
}

// applyFieldProjection rewrites SELECT * to select only the specified fields,
// reconstructing `raw` as a space-joined concatenation of the selected fields.
func applyFieldProjection(sql string, fields []int) string {
	// Build the field extraction expressions.
	var fieldExprs []string
	for _, f := range fields {
		if f < 0 {
			// Open-ended range: select from field |f| to end.
			// DuckDB: list_slice(fields, start, len(fields))
			start := -f
			fieldExprs = append(fieldExprs, fmt.Sprintf(
				"array_to_string(fields[%d:], ' ')", start))
		} else {
			fieldExprs = append(fieldExprs, fmt.Sprintf(
				"list_extract(fields, %d)", f))
		}
	}

	// Reconstruct raw as concatenation of selected fields.
	var rawExpr string
	if len(fieldExprs) == 1 {
		rawExpr = fieldExprs[0]
	} else {
		rawExpr = "concat_ws(' ', " + strings.Join(fieldExprs, ", ") + ")"
	}

	// Build new field list for the projected fields array.
	var projectedFieldsList string
	if len(fields) == 1 && fields[0] > 0 {
		projectedFieldsList = fmt.Sprintf("[list_extract(fields, %d)]", fields[0])
	} else {
		var listParts []string
		for _, f := range fields {
			if f < 0 {
				start := -f
				listParts = append(listParts, fmt.Sprintf(
					"fields[%d:]", start))
			} else {
				listParts = append(listParts, fmt.Sprintf(
					"[list_extract(fields, %d)]", f))
			}
		}
		projectedFieldsList = "list_concat(" + strings.Join(listParts, ", ") + ")"
		if len(listParts) == 1 {
			projectedFieldsList = listParts[0]
		}
	}

	selectClause := fmt.Sprintf(
		"SELECT line_num, %s as raw, %s as fields, ts, level, app, instance, customer, payload, host_group",
		rawExpr, projectedFieldsList)

	return strings.Replace(sql, "SELECT *", selectClause, 1)
}

// applyJSONProjection rewrites SELECT * to extract only the needed JSON keys
// from the payload column, and sets raw to the JSON payload text so jq compute
// stages receive JSON input.
func applyJSONProjection(sql string, keys []string) string {
	// Build a JSON object with only the needed keys.
	var keyExprs []string
	for _, k := range keys {
		keyExprs = append(keyExprs, fmt.Sprintf(
			"'%s', json_extract(payload, '$.%s')", k, k))
	}
	payloadExpr := "json_object(" + strings.Join(keyExprs, ", ") + ")"

	// Set raw to the full payload cast as text, so jq stages get JSON input.
	selectClause := fmt.Sprintf(
		"SELECT line_num, CAST(payload AS TEXT) as raw, fields, ts, level, app, instance, customer, %s as payload, host_group",
		payloadExpr)

	return strings.Replace(sql, "SELECT *", selectClause, 1)
}
