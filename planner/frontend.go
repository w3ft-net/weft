package planner

import (
	"strings"

	"github.com/graybeardcorp/warp/pkg/awk"
)

// lowerStage runs a stage's frontend: it lowers the stage, from its own
// syntax tree, into IR nodes. Unknown commands, flags and constructs
// become an opaqueNode with the reason.
func lowerStage(s stageInfo, index int) *loweredStage {
	ls := &loweredStage{stageInfo: s, index: index}
	a := at(index)
	switch s.cmd {
	case "awk":
		ls.nodes = lowerAwk(ls, a)
	case "jq":
		ls.nodes = lowerJq(ls, a)
	case "grep":
		ls.rowwise = grepIsRowFilter(s.args)
		pred, pat, why := grepFilter(s.args)
		if pred == "" {
			ls.nodes = []node{&opaqueNode{at: a, why: why}}
		} else {
			ls.nodes = []node{&filterNode{at: a, pred: pred, exact: true, follow: &pat, lineWise: true}}
		}
	case "sed":
		_, ls.rowwise = sedAddressPrint(s.args)
		if pred := trySedPushdown(s.args); pred != "" {
			ls.nodes = []node{&filterNode{at: a, pred: pred, exact: true, lineWise: true}}
		} else {
			ls.nodes = []node{&opaqueNode{at: a, why: because(reasonSedProgram)}}
		}
	case "head", "tail":
		_, ls.rowwise = rowStageLineCount(s.args)
		order := "ts ASC"
		if s.cmd == "tail" {
			order = "ts DESC"
		}
		ls.nodes = []node{&limitNode{at: a, n: parseLineCount(s.args), order: order}}
	case "sort":
		sn := &sortNode{at: a}
		for _, f := range s.args {
			if !strings.HasPrefix(f, "-") {
				// A flag's value (-k 2) or a file operand, which sort
				// reads instead of its input: either way, not the
				// lines' bytes in order.
				sn.keyed = true
				continue
			}
			for _, flag := range f[1:] {
				switch flag {
				case 'r':
					sn.reverse = true
				case 'u':
					sn.unique = true
				default:
					sn.keyed = true
				}
			}
		}
		ls.nodes = []node{sn}
	case "uniq":
		switch strings.Join(s.args, " ") {
		case "":
			ls.nodes = []node{&uniqNode{at: a}}
		case "-c":
			ls.nodes = []node{&uniqNode{at: a, count: true}}
		default:
			ls.nodes = []node{&opaqueNode{at: a, why: reason{code: reasonFlag, detail: s.args[0]}}}
		}
	case "wc":
		if len(s.args) == 1 && s.args[0] == "-l" {
			ls.nodes = []node{&countNode{at: a}}
		} else {
			ls.nodes = []node{&opaqueNode{at: a, why: reason{code: reasonNoFrontend, detail: s.src}}}
		}
	case "cut":
		if expr, why := cutProjection(s.args); why.code == reasonNone {
			ls.nodes = []node{&projectNode{at: a, expr: expr}}
		} else {
			ls.nodes = []node{&opaqueNode{at: a, why: why}}
		}
	default:
		ls.nodes = []node{&opaqueNode{at: a, why: reason{code: reasonNoFrontend, detail: s.cmd}}}
	}
	return ls
}

// lowerAwk is awk's frontend. Flags (-F, -v) change how awk splits and
// what its variables hold, so only a bare program lowers.
func lowerAwk(ls *loweredStage, a at) []node {
	src, ok := awkProgramArg(ls.args)
	if !ok {
		// -f: the program isn't on the command line, so assume it reads
		// meta-variables; projecting them only costs columns.
		ls.readsMeta = true
		return []node{&opaqueNode{at: a, why: reason{code: reasonFlag, detail: "-f"}}}
	}
	prog, err := awk.Parse(src)
	if err != nil {
		ls.readsMeta = true
		return []node{&opaqueNode{at: a, why: because(reasonParse)}}
	}
	ls.readsMeta = astContains(prog, func(n any) bool {
		id, ok := n.(*awk.IdentExpr)
		return ok && AwkMetaVars[id.Name] != ""
	})
	if len(ls.args) != 1 {
		return []node{&opaqueNode{at: a, why: reason{code: reasonFlag, detail: ls.args[0]}}}
	}
	ls.rowwise = awkIsRowFilter(prog)

	pd, why := tryAwkPushdown(prog)
	switch {
	case pd == nil:
		if expr, ok := awkProjection(prog); ok {
			return []node{&projectNode{at: a, expr: expr}}
		}
		if len(prog.Rules) == 1 && prog.Rules[0].Pattern == nil {
			// One unpatterned rule: say why its action isn't a projection.
			why = because(reasonAwkProjection)
			if astContains(prog, isAwkComputedField) {
				why = because(reasonAwkComputedField)
			}
		}
		return []node{&opaqueNode{at: a, why: why}}
	case pd.limit > 0 || pd.offset > 0:
		return []node{&limitNode{at: a, n: pd.limit, offset: pd.offset}}
	case pd.exact:
		return []node{&filterNode{at: a, pred: pd.predicate, exact: true}}
	}
	filterWhy := pd.why
	if filterWhy.code == reasonNone {
		filterWhy = why
	}
	return []node{
		&filterNode{at: a, pred: pd.predicate, why: filterWhy},
		&opaqueNode{at: a, why: why},
	}
}

// awkProgramArg finds the program among awk's arguments, past -F, -v
// and their values. ok is false for -f, which reads it from a file.
func awkProgramArg(args []string) (string, bool) {
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "-f" || strings.HasPrefix(arg, "-f"):
			return "", false
		case arg == "-F" || arg == "-v":
			i++
		case arg == "--":
		case strings.HasPrefix(arg, "-F") || strings.HasPrefix(arg, "-v"):
		default:
			return arg, true
		}
	}
	return "", true
}

// lowerJq is jq's frontend (docs/plans/planner-ir.md, "jq"). Its leading
// selects are a filter, exact for a select chain whose selects all
// lower. What follows them is a projection when SQL computes all of it,
// and otherwise the stage runs whole, its selects a prefilter.
func lowerJq(ls *loweredStage, a at) []node {
	p, why := analyzeJq(ls.args)
	if p == nil {
		ls.readsMeta = true
		return []node{&opaqueNode{at: a, why: why}}
	}
	ls.readsMeta = readsJqMetaVar(p.expr)
	ls.rowwise = p.selectChain()
	if p.slurp {
		return []node{&opaqueNode{at: a, why: because(reasonJqSlurp)}}
	}
	pred := strings.Join(p.selects, " AND ")
	if p.allLower && p.tail == nil {
		return []node{&filterNode{at: a, pred: pred, exact: true}}
	}
	if p.allLower {
		if sql, ok := jqValueToSQL(p.tail); ok {
			var nodes []node
			if pred != "" {
				nodes = append(nodes, &filterNode{at: a, pred: pred, exact: true})
			}
			return append(nodes, &projectNode{at: a, expr: wrapJqProjection(sql, p.raw)})
		}
	}
	why = because(reasonJqTail)
	if !p.allLower {
		why = because(reasonJqSelect)
	}
	if pred == "" {
		return []node{&opaqueNode{at: a, why: why}}
	}
	return []node{
		&filterNode{at: a, pred: pred, why: why},
		&opaqueNode{at: a, why: why},
	}
}
