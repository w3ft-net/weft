package planner

import "fmt"

// trySedPushdown lowers `sed -n '/re/p'`, the one sed program that's a
// filter, to a regex predicate. Returns "" otherwise.
func trySedPushdown(args []string) string {
	re, ok := sedAddressPrint(args)
	if !ok {
		return ""
	}
	// sed regex addresses use BRE; map to DuckDB's regexp_matches
	// (RE2 substring/find). DuckDB's `~` operator is whole-string
	// match (SIMILAR TO semantics) and silently returns no rows for
	// the substring patterns sed users actually write.
	return fmt.Sprintf("regexp_matches(raw, '%s')", escapeSQLString(re))
}
