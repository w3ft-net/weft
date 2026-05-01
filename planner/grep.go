package planner

import "fmt"

// parseGrepArgs extracts grep flags and pattern from arguments.
func parseGrepArgs(args []string) (pattern string, ignoreCase, invert bool) {
	for _, arg := range args {
		if arg == "-v" {
			invert = true
		} else if arg == "-i" {
			ignoreCase = true
		} else if arg == "-iv" || arg == "-vi" {
			invert = true
			ignoreCase = true
		} else if len(arg) > 1 && arg[0] == '-' {
			for _, c := range arg[1:] {
				switch c {
				case 'i':
					ignoreCase = true
				case 'v':
					invert = true
				}
			}
		} else {
			pattern = arg
		}
	}
	return
}

// tryGrepPushdown extracts a SQL predicate from grep arguments.
// Returns the SQL WHERE fragment, or "" if not pushable.
//
// grep patterns are real regex (BRE/ERE depending on flags); we map
// them to DuckDB's `~` operator (RE2). For typical patterns the
// flavors agree; for BRE-specific shapes (e.g. `\(...\)` for groups)
// the pushdown is best-effort and may differ — operators reaching
// for those edges should use grep -E or awk for semantic clarity.
//
// The previous LIKE-substring encoding silently mismatched any
// pattern with regex specials (`^ERROR` was matched as the literal
// string `^ERROR`, never appearing in real text).
func tryGrepPushdown(args []string) string {
	pattern := ""
	ignoreCase := false
	invert := false

	for _, arg := range args {
		if arg == "-v" {
			invert = true
		} else if arg == "-i" {
			ignoreCase = true
		} else if arg == "-iv" || arg == "-vi" {
			invert = true
			ignoreCase = true
		} else if len(arg) > 1 && arg[0] == '-' {
			for _, c := range arg[1:] {
				switch c {
				case 'i':
					ignoreCase = true
				case 'v':
					invert = true
				default:
					return "" // unknown flag — can't push down
				}
			}
		} else {
			pattern = arg
		}
	}
	if pattern == "" {
		return ""
	}

	// DuckDB's `~` operator is whole-string regex match (SIMILAR TO
	// semantics), not PostgreSQL-style substring match — and `~*` /
	// `!~*` don't exist as scalar functions at all. Use regexp_matches
	// which is the substring/find form the user expects from grep.
	// Third arg "i" enables case-insensitive matching.
	expr := fmt.Sprintf("regexp_matches(raw, '%s')", escapeSQLString(pattern))
	if ignoreCase {
		expr = fmt.Sprintf("regexp_matches(raw, '%s', 'i')", escapeSQLString(pattern))
	}
	if invert {
		expr = "NOT " + expr
	}
	return expr
}
