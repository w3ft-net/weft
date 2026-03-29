package planner

import "fmt"

// tryGrepPushdown extracts a SQL predicate from grep arguments.
// Returns the SQL WHERE fragment, or "" if not pushable.
func tryGrepPushdown(args []string) string {
	invert := false
	ignoreCase := false
	pattern := ""

	for _, arg := range args {
		if arg == "-v" {
			invert = true
		} else if arg == "-i" {
			ignoreCase = true
		} else if arg == "-iv" || arg == "-vi" {
			invert = true
			ignoreCase = true
		} else if len(arg) > 1 && arg[0] == '-' {
			// Parse combined flags like -iv.
			for _, c := range arg[1:] {
				switch c {
				case 'i':
					ignoreCase = true
				case 'v':
					invert = true
				default:
					return "" // unknown flag
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
