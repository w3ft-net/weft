package planner

import "fmt"

// grepFilter lowers a grep stage to a SQL predicate, with the pattern
// live tail applies to the same stage. pred is "" when it doesn't lower,
// and why says why.
//
// grep patterns are real regex (BRE/ERE depending on flags); we map
// them to DuckDB's regexp_matches (RE2). For typical patterns the
// flavors agree; for BRE-specific shapes (e.g. `\(...\)` for groups)
// the pushdown is best-effort and may differ — operators reaching
// for those edges should use grep -E or awk for semantic clarity.
func grepFilter(args []string) (pred string, pat GrepPattern, why reason) {
	for _, arg := range args {
		if len(arg) > 1 && arg[0] == '-' {
			for _, c := range arg[1:] {
				switch c {
				case 'i':
					pat.IgnoreCase = true
				case 'v':
					pat.Invert = true
				case 'E':
					// The grep that runs these lines compiles every pattern as
					// RE2, which is extended syntax already, so -E changes
					// nothing there or here.
				default:
					return "", pat, reason{code: reasonFlag, detail: arg}
				}
			}
		} else {
			pat.Pattern = arg
		}
	}
	if pat.Pattern == "" {
		return "", pat, because(reasonGrepPattern)
	}

	// DuckDB's `~` operator is whole-string regex match (SIMILAR TO
	// semantics), not PostgreSQL-style substring match — and `~*` /
	// `!~*` don't exist as scalar functions at all. Use regexp_matches
	// which is the substring/find form the user expects from grep.
	// Third arg "i" enables case-insensitive matching.
	pred = fmt.Sprintf("regexp_matches(raw, '%s')", escapeSQLString(pat.Pattern))
	if pat.IgnoreCase {
		pred = fmt.Sprintf("regexp_matches(raw, '%s', 'i')", escapeSQLString(pat.Pattern))
	}
	if pat.Invert {
		pred = "NOT " + pred
	}
	return pred, pat, reason{}
}
