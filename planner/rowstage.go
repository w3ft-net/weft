package planner

import (
	"strconv"
	"strings"

	"github.com/graybeardcorp/warp/pkg/awk"
	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/graybeardcorp/warp/pkg/sed"
	"github.com/w3ft-net/weft/backends/logs/dialect"
)

// A row stage passes rows through unchanged: it keeps or drops each
// one (a filter) or keeps a count of them (head/tail -n N). A stage
// after it still reads rows, with their payload and meta-variables,
// rather than text. That holds whether or not the stage pushed down, so
// when row stages reach compute ahead of a stage that reads row
// metadata, the engine runs them per row (QueryPlan.RowStages) instead
// of as a text pipe.

// grepIsRowFilter accepts one pattern and only the flags that keep a
// matching line as it is: -i, -v, -E.
func grepIsRowFilter(args []string) bool {
	patterns := 0
	for _, a := range args {
		if len(a) > 1 && a[0] == '-' {
			if strings.Trim(a[1:], "ivE") != "" {
				return false
			}
			continue
		}
		patterns++
	}
	return patterns == 1
}

// awkIsRowFilter accepts a program that is one pattern with the default
// print, evaluated on each record alone: no BEGIN/END, no record
// counters, no getline, and a pattern without side effects, so running
// it on one row at a time means what running it on the stream does.
func awkIsRowFilter(prog *awk.Program) bool {
	if len(prog.Functions) > 0 || len(prog.Rules) != 1 {
		return false
	}
	r := &prog.Rules[0]
	switch r.Pattern.(type) {
	case *awk.ExprPattern, *awk.RegexPattern:
	default:
		return false
	}
	if r.Action != nil && !isDefaultPrintAction(r.Action) {
		return false
	}
	return !astContains(r.Pattern, func(n any) bool {
		switch e := n.(type) {
		case *awk.AssignExpr, *awk.IncDecExpr, *awk.GetlineExpr:
			return true
		case *awk.IdentExpr:
			return e.Name == "NR" || e.Name == "FNR"
		case *awk.CallExpr:
			return !awkPureBuiltins[e.Name]
		}
		return false
	})
}

// awkPureBuiltins read their arguments and nothing else.
var awkPureBuiltins = map[string]bool{
	"length": true, "substr": true, "index": true, "tolower": true,
	"toupper": true, "int": true, "sprintf": true,
}

// sedAddressFilter recognizes the sed programs that are a line filter:
// `-n '/re/p'` and `'/re/d'`, each optionally negated with `!`. It returns
// the regex and whether lines matching it are dropped rather than kept.
// Without -n a print repeats the matching lines, and with -n a delete prints
// nothing, so neither is a filter.
func sedAddressFilter(args []string) (re string, drop, ok bool) {
	hasN := false
	script := ""
	for _, a := range args {
		switch {
		case a == "-n":
			hasN = true
		case strings.HasPrefix(a, "-") || script != "":
			return "", false, false
		default:
			script = a
		}
	}
	if script == "" {
		return "", false, false
	}
	prog, err := sed.Compile(script)
	if err != nil || len(prog.Commands) != 1 {
		return "", false, false
	}
	cmd := prog.Commands[0]
	if cmd.Addr1 == nil || cmd.Addr1.Type != sed.AddrRegex || cmd.Addr2 != nil {
		return "", false, false
	}
	switch {
	case cmd.Type == sed.CmdPrint && hasN:
		drop = false
	case cmd.Type == sed.CmdDelete && !hasN:
		drop = true
	default:
		return "", false, false
	}
	return cmd.Addr1.Regex, drop != cmd.Negated, true
}

// rowStageLineCount accepts head/tail's `-n N` and `-N`.
func rowStageLineCount(args []string) (int, bool) {
	var s string
	switch {
	case len(args) == 2 && args[0] == "-n":
		s = args[1]
	case len(args) == 1 && strings.HasPrefix(args[0], "-n"):
		s = args[0][2:]
	case len(args) == 1 && strings.HasPrefix(args[0], "-"):
		s = args[0][1:]
	case len(args) == 0:
		return 10, true
	default:
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0
}

// splitRowStages separates the leading row stages from the rest when
// the rows they pass along still matter: a stage among them, or the
// first stage after them, reads row metadata. Otherwise there's nothing
// to carry and every stage runs as a plain text pipe.
func splitRowStages(stages []*loweredStage) (rows, rest []*loweredStage) {
	n := 0
	for n < len(stages) && stages[n].rowwise {
		n++
	}
	if n == 0 {
		return nil, stages
	}
	// The first stage reads rows even in a plain pipe: an awk there gets
	// its meta-variables from the per-record hook. Every other reader,
	// and a jq select chain anywhere (it must emit the row's raw line),
	// needs the rows carried.
	needed := false
	for i, s := range stages[:min(n+1, len(stages))] {
		if s.readsRowMetadata() && !(i == 0 && s.cmd == "awk") {
			needed = true
		}
	}
	if !needed {
		return nil, stages
	}
	return stages[:n], stages[n:]
}

// IsRowStage reports whether a stage, as written, passes rows through
// unchanged. The differential harness builds its reference plan from it.
func IsRowStage(stage string) bool {
	sc, ok := parseOneStage(stage)
	return ok && lowerStage(parseStage(sc), 1, lowering{d: dialect.DuckDB}).rowwise
}

// parseOneStage parses a stage's text as the one command it is.
func parseOneStage(stage string) (*bash.SimpleCommand, bool) {
	script, err := bash.Parse(stage)
	if err != nil || len(script.Commands) != 1 || len(script.Commands[0].First.Commands) != 1 {
		return nil, false
	}
	sc, ok := script.Commands[0].First.Commands[0].(*bash.SimpleCommand)
	return sc, ok
}
