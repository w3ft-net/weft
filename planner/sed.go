package planner

// trySedPushdown lowers the sed programs that are a line filter (see
// sedAddressFilter) to a regex predicate. Returns "" otherwise.
func trySedPushdown(args []string) sqlExpr {
	re, drop, ok := sedAddressFilter(args)
	if !ok {
		return nil
	}
	// sed regex addresses use BRE; map to DuckDB's regexp_matches
	// (RE2 substring/find). DuckDB's `~` operator is whole-string
	// match (SIMILAR TO semantics) and silently returns no rows for
	// the substring patterns sed users actually write.
	var pred sqlExpr = regexExpr{col: colRef("raw"), pattern: re}
	if drop {
		return notExpr{pred}
	}
	return pred
}
