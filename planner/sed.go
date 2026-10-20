package planner

import "fmt"

// trySedPushdown lowers the sed programs that are a line filter (see
// sedAddressFilter) to a regex predicate. Returns "" otherwise.
func trySedPushdown(args []string) string {
	re, drop, ok := sedAddressFilter(args)
	if !ok {
		return ""
	}
	// sed regex addresses use BRE; map to DuckDB's regexp_matches
	// (RE2 substring/find). DuckDB's `~` operator is whole-string
	// match (SIMILAR TO semantics) and silently returns no rows for
	// the substring patterns sed users actually write.
	pred := fmt.Sprintf("regexp_matches(raw, '%s')", escapeSQLString(re))
	if drop {
		return "NOT " + pred
	}
	return pred
}
