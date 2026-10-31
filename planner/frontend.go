package planner

import (
	"strings"

	"github.com/graybeardcorp/warp/pkg/awk"
)

// lowerStage runs a stage's frontend: it lowers the stage, from its own
// syntax tree, into IR nodes. Unknown commands, flags and constructs
// become an opaqueNode with the reason.
func lowerStage(s stageInfo, index int, typed bool) *loweredStage {
	ls := &loweredStage{stageInfo: s, index: index}
	a := at(index)
	switch s.cmd {
	case "awk":
		ls.nodes = lowerAwk(ls, a, typed)
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
		_, _, ls.rowwise = sedAddressFilter(s.args)
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
		other := false
		for _, f := range s.args {
			if !strings.HasPrefix(f, "-") {
				// A flag's value (-k 2) or a file operand, which sort
				// reads instead of its input: either way, not the
				// lines' bytes in order.
				sn.keyed, other = true, true
				continue
			}
			for _, flag := range f[1:] {
				switch flag {
				case 'r':
					sn.reverse = true
				case 'u':
					sn.unique = true
				case 'n':
					sn.keyed, sn.numeric = true, true
				default:
					sn.keyed, other = true, true
				}
			}
		}
		sn.numeric = sn.numeric && !other
		if sa := parseSortArgs(s.args); sa.plain {
			sn.plain, sn.order = true, sa.order
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
		if len(s.args) == 1 && (s.args[0] == "-l" || s.args[0] == "-c") {
			ls.nodes = []node{&countNode{at: a, bytes: s.args[0] == "-c"}}
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
func lowerAwk(ls *loweredStage, a at, typed bool) []node {
	src, ok := awkProgramArg(ls.args)
	if !ok {
		// -f: the program isn't on the command line, so assume it reads
		// meta-variables; projecting them only costs columns.
		ls.meta = allMeta
		ls.fieldsWhy = "-f cannot be optimized"
		return []node{&opaqueNode{at: a, why: reason{code: reasonFlag, detail: "-f"}}}
	}
	prog, err := awk.Parse(src)
	if err != nil {
		ls.meta = allMeta
		return []node{&opaqueNode{at: a, why: because(reasonParse)}}
	}
	ls.meta = awkMetaRead(prog)
	if len(ls.args) != 1 {
		// The stage runs verbatim in compute, but it can still be cut down to
		// the fields it reads if its flags leave the split alone.
		if ls.fieldsWhy = awkFlagsWhy(ls.args); ls.fieldsWhy == "" {
			if reads, why := awkFieldReads(prog); why == "" {
				ls.fields = &reads
			} else {
				ls.fieldsWhy = why
			}
		}
		return []node{&opaqueNode{at: a, why: reason{code: reasonFlag, detail: ls.args[0]}}}
	}
	ls.rowwise = awkIsRowFilter(prog)
	if reads, why := awkFieldReads(prog); why == "" {
		ls.fields = &reads
	} else {
		ls.fieldsWhy = why
	}

	pd, why := tryAwkPushdown(prog, typed)
	if pd != nil && (pd.limit > 0 || pd.offset > 0) {
		return []node{&limitNode{at: a, n: pd.limit, offset: pd.offset}}
	}
	nodes := awkRowStage(prog, pd, why, typed).nodes(a)
	if agg, ok := awkKeyed(prog); ok && len(nodes) == 1 {
		if o, ok := nodes[0].(*opaqueNode); ok {
			return []node{&aggregateNode{at: a, agg: agg, typed: typed, why: o.why}}
		}
	}
	return nodes
}

// awkRowStage says what an awk program does to a row in the terms every
// row stage shares: pd is tryAwkPushdown's predicate and why its reason.
func awkRowStage(prog *awk.Program, pd *awkPushdown, why reason, typed bool) rowStage {
	if pd == nil {
		if expr, ok := awkProjection(prog); ok {
			return rowStage{proj: expr}
		}
		if len(prog.Rules) == 1 && prog.Rules[0].Pattern == nil {
			// One unpatterned rule: say why its action isn't a projection.
			why = because(reasonAwkProjection)
			if astContains(prog, isAwkComputedField) {
				why = because(reasonAwkComputedField)
			}
		}
		return rowStage{opaque: true, why: why}
	}
	rs := rowStage{pred: pd.predicate, predExact: pd.exact, typed: pd.typed}
	if pd.exact {
		return rs
	}
	// A pattern SQL evaluates exactly, over an action it can print.
	if len(prog.Rules) == 1 {
		if pat := extractAwkPattern(prog.Rules[0].Pattern, typed); pat != nil && pat.exact {
			if expr, ok := awkActionProjection(prog.Rules[0].Action); ok {
				rs.predExact, rs.proj = true, expr
				return rs
			}
		}
	}
	rs.opaque, rs.why, rs.predWhy = true, why, pd.why
	return rs
}

// awkFlagsWhy is why an awk stage's flags keep it from being cut down to the
// fields it reads, "" if they don't. -F changes how a line splits, and so does
// -v setting FS, RS or NF, so the whitespace-split fields the shard stores are
// not its fields. Any other -v just sets a variable. It reads the flags as
// awkProgramArg does, up to the program.
func awkFlagsWhy(args []string) string {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case strings.HasPrefix(a, "-F"):
			return "-F cannot be optimized"
		case strings.HasPrefix(a, "-v"):
			assign := strings.TrimPrefix(a, "-v")
			if assign == "" && i+1 < len(args) {
				i++
				assign = args[i]
			}
			switch name, _, _ := strings.Cut(assign, "="); name {
			case "FS", "RS", "NF":
				return "-v " + name + " cannot be optimized"
			}
		case a == "--":
		default:
			return "" // the program
		}
	}
	return ""
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
		ls.meta = allMeta
		return []node{&opaqueNode{at: a, why: why}}
	}
	ls.meta = jqMetaRead(p.expr)
	ls.rowwise = p.selectChain()
	if p.slurp {
		return []node{&opaqueNode{at: a, why: because(reasonJqSlurp)}}
	}
	return jqRowStage(p).nodes(a)
}

// jqRowStage says what a jq program does to a row in the terms every row
// stage shares.
func jqRowStage(p *jqProgram) rowStage {
	pred := strings.Join(p.selects, " AND ")
	if p.allLower && p.tail == nil && len(p.subject) == 0 {
		return rowStage{pred: pred, predExact: true, typed: p.typed}
	}
	if p.allLower {
		// The subject is what the program holds. A tail reads its own
		// paths from the payload, so it only lowers with no subject.
		var sql string
		ok := false
		switch {
		case p.tail == nil:
			pred = strings.Join(append(p.subject.guards(), p.selects...), " AND ")
			sql, ok = jqPrintPathSQL(p.subject, p.raw), true
		case len(p.subject) == 0:
			if sql, ok = jqValueToSQL(p.tail); ok {
				// Every path it reads must be readable, or jq errors and
				// drops the row.
				pred = strings.Join(append(jqValueGuards(p.tail), p.selects...), " AND ")
				sql = jqPrintTailSQL(p.tail, sql, p.raw)
			}
		}
		if ok {
			return rowStage{pred: pred, predExact: true, proj: sql}
		}
	}
	why := because(reasonJqTail)
	if !p.allLower {
		why = because(reasonJqSelect)
	}
	return rowStage{pred: pred, opaque: true, why: why, typed: p.typed}
}

// rowStage is what a row stage (awk, jq) does to each row, in the terms
// SQL can act on. A frontend fills it in; nodes is the one place it
// becomes IR, so every language folds a filter and a projection the same
// way.
type rowStage struct {
	pred      string      // SQL predicate on the row, or "" for none
	predExact bool        // pred is exactly the stage's filter, not just a prefilter
	proj      string      // SQL for the text the stage prints, or "" if rows pass through
	opaque    bool        // the stage does more than pred and proj, so it still runs
	why       reason      // why the stage still runs
	predWhy   reason      // why pred is only a prefilter; why if unset
	typed     []typedLeaf // comparisons in pred with a simpler typed form
}

func (rs rowStage) nodes(a at) []node {
	if rs.opaque {
		if rs.pred == "" {
			return []node{&opaqueNode{at: a, why: rs.why}}
		}
		return []node{
			&filterNode{at: a, pred: rs.pred, why: cmpReason(rs.predWhy, rs.why), typed: rs.typed},
			&opaqueNode{at: a, why: rs.why},
		}
	}
	var nodes []node
	if rs.pred != "" || rs.proj == "" {
		nodes = append(nodes, &filterNode{at: a, pred: rs.pred, exact: true, typed: rs.typed})
	}
	if rs.proj != "" {
		nodes = append(nodes, &projectNode{at: a, expr: rs.proj})
	}
	return nodes
}

// cmpReason is the first reason that says anything.
func cmpReason(rs ...reason) reason {
	for _, r := range rs {
		if r.code != reasonNone {
			return r
		}
	}
	return reason{}
}
