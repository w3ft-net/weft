package planner

import (
	"fmt"
	"strings"

	"github.com/kevlarcon/warp/pkg/sed"
)

// trySedPushdown attempts to extract a pushable predicate from sed arguments.
// Only handles the case: sed -n '/pattern/p' (address-filtered print with
// default output suppressed).
// Returns the SQL WHERE fragment, or "" if not pushable.
func trySedPushdown(args []string) string {
	// We need -n flag and a script argument.
	hasN := false
	script := ""

	for _, arg := range args {
		if arg == "-n" {
			hasN = true
		} else if !strings.HasPrefix(arg, "-") {
			script = arg
		}
	}

	if !hasN || script == "" {
		return ""
	}

	prog, err := sed.Compile(script)
	if err != nil {
		return ""
	}

	// Only handle a single print command with a regex address.
	if len(prog.Commands) != 1 {
		return ""
	}

	cmd := prog.Commands[0]
	if cmd.Type != sed.CmdPrint {
		return ""
	}
	if cmd.Addr1 == nil || cmd.Addr1.Type != sed.AddrRegex {
		return ""
	}
	if cmd.Addr2 != nil {
		return "" // range addresses not supported yet
	}

	// sed regex addresses use BRE; map to DuckDB's `~` (RE2). Same
	// flavor caveat as grep — for the substring patterns most users
	// write the flavors agree, and the previous LIKE-substring
	// encoding silently mismatched real regex specials.
	return fmt.Sprintf("raw ~ '%s'", escapeSQLString(cmd.Addr1.Regex))
}
