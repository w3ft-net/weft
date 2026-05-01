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

	op := "~"
	switch {
	case ignoreCase && invert:
		op = "!~*"
	case ignoreCase:
		op = "~*"
	case invert:
		op = "!~"
	}

	return fmt.Sprintf("raw %s '%s'", op, escapeSQLString(pattern))
}
