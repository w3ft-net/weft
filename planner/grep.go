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

	likeOp := "LIKE"
	if ignoreCase && invert {
		likeOp = "NOT ILIKE"
	} else if ignoreCase {
		likeOp = "ILIKE"
	} else if invert {
		likeOp = "NOT LIKE"
	}

	return fmt.Sprintf("raw %s '%%%s%%'", likeOp, escapeSQLLike(pattern))
}
