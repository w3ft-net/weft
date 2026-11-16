package planner

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/graybeardcorp/warp/pkg/awk"
	"github.com/w3ft-net/weft/backends/logs/dialect"
)

// maxProjectedField bounds how many positions the field form selects: each
// position is a column in the shard's query.
const maxProjectedField = 32

// awkFieldReads is which fields an awk program reads, when it reads nothing
// else of the record: the constant positions $1, $2, ... it names, in order,
// and whether it reads $NF or NF. ok is false when anything else could observe the
// record's text, which a record with only those fields in place would show
// differently: $0 in any form (a bare print, a bare length, a /re/ that
// matches it, a rule with no action, sub or gsub's default target), a computed position, an
// assignment to a field or to NF, FS or RS, getline, a user function, or a
// construct this doesn't know. The program is walked as a whitelist: a node
// it doesn't recognize is a reason to say no, and why says which.
func awkFieldReads(prog *awk.Program) (reads fieldReads, why string) {
	if len(prog.Functions) > 0 {
		return fieldReads{}, "it defines a function"
	}
	// A /re/ is a match against $0 unless it is used as a regex: the right of
	// ~, the regex argument of sub, gsub, match or split.
	asRegex := map[*awk.RegexLit]bool{}
	regexArg := map[string]int{"sub": 0, "gsub": 0, "match": 1, "split": 2}
	astContains(prog, func(n any) bool {
		switch e := n.(type) {
		case *awk.MatchExpr:
			if r, ok := e.Pattern.(*awk.RegexLit); ok {
				asRegex[r] = true
			}
		case *awk.CallExpr:
			if i, ok := regexArg[e.Name]; ok && i < len(e.Args) {
				if r, ok := e.Args[i].(*awk.RegexLit); ok {
					asRegex[r] = true
				}
			}
		}
		return false
	})

	seen := map[int]bool{}
	// stop records the first reason the program reads more than its fields.
	stop := func(reason string) bool {
		if why == "" {
			why = reason
		}
		return true
	}
	astContains(prog, func(n any) bool {
		switch e := n.(type) {
		case *awk.FieldExpr:
			if id, ok := e.Index.(*awk.IdentExpr); ok && id.Name == "NF" {
				reads.last = true
				return false
			}
			idx, ok := e.Index.(*awk.NumberLit)
			if !ok || idx.Value != float64(int(idx.Value)) || idx.Value < 1 || idx.Value > maxProjectedField {
				return stop("it reads $0 or a field position it can't name ahead of time")
			}
			seen[int(idx.Value)] = true
		case *awk.IdentExpr:
			switch e.Name {
			case "length":
				return stop("a bare length reads the whole line")
			case "FS", "RS":
				return stop("it changes how lines split")
			case "NF":
				reads.nf = true
			}
		case *awk.Rule:
			// No action is { print $0 }.
			if e.Action == nil {
				return stop("a rule with no action prints the whole line")
			}
		case *awk.RegexLit:
			if !asRegex[e] {
				return stop("a /regex/ on its own matches the whole line")
			}
		case *awk.PrintStmt:
			if len(e.Args) == 0 {
				return stop("print with no arguments prints the whole line")
			}
			if e.Dest != nil {
				return stop("it redirects output")
			}
		case *awk.PrintfStmt:
			if e.Dest != nil {
				return stop("it redirects output")
			}
		case *awk.CallExpr:
			if !awkCallKeepsRecord(e) {
				return stop(e.Name + " reads the whole line, or isn't known to read only its arguments")
			}
		case *awk.AssignExpr:
			if awkWritesRecord(e.Target) {
				return stop("it assigns to a field, NF, FS or RS")
			}
		case *awk.AssignStmt:
			if awkWritesRecord(e.Target) {
				return stop("it assigns to a field, NF, FS or RS")
			}
		case *awk.IncDecExpr:
			if awkWritesRecord(e.Operand) {
				return stop("it assigns to a field, NF, FS or RS")
			}
		default:
			if !awkNodeIsPlain(n) {
				return stop("it uses a construct not known to leave the line alone")
			}
		}
		return false
	})
	if why != "" {
		return fieldReads{}, why
	}
	for p := range seen {
		reads.positions = append(reads.positions, p)
	}
	slices.Sort(reads.positions)
	return reads, ""
}

// FieldSpec is which fields a compute stage reads, so the shards send only
// those (QueryPlan.FieldSpec): the positions ($1, $3, ...) in order, whether it
// reads the last field ($NF), and whether it reads the count (NF).
type FieldSpec struct {
	Positions []int `json:"positions,omitempty"`
	Last      bool  `json:"last,omitempty"`
	NF        bool  `json:"nf,omitempty"`
}

func (r fieldReads) spec() *FieldSpec {
	return &FieldSpec{Positions: r.positions, Last: r.last, NF: r.nf}
}

// fieldReads is what awkFieldReads found: the constant positions read, in
// order, whether $NF is, and whether NF is.
type fieldReads struct {
	positions []int
	last, nf  bool
}

// awkCallKeepsRecord is whether a call to a built-in function reads only what
// it is handed, not $0: length needs an argument, and sub and gsub a target
// that isn't a field (a field would be assigned).
func awkCallKeepsRecord(c *awk.CallExpr) bool {
	switch c.Name {
	case "length":
		return len(c.Args) == 1
	case "sub", "gsub":
		if len(c.Args) < 3 {
			return false
		}
		_, field := c.Args[2].(*awk.FieldExpr)
		return !field
	case "substr", "index", "split", "match", "sprintf", "tolower", "toupper", "int",
		"sin", "cos", "sqrt", "log", "exp", "atan2", "rand", "srand":
		return true
	}
	return false
}

// awkWritesRecord is whether an assignment target is the record: a field, or
// NF, FS or RS.
func awkWritesRecord(target awk.Expr) bool {
	switch t := target.(type) {
	case *awk.FieldExpr:
		return true
	case *awk.IdentExpr:
		return t.Name == "NF" || t.Name == "FS" || t.Name == "RS"
	}
	return false
}

// awkNodeIsPlain is whether a syntax node is one that can't touch the record
// on its own: its operands are visited separately. Anything not listed, and
// anything added to warp's AST later, is not.
func awkNodeIsPlain(n any) bool {
	t := reflect.TypeOf(n)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if !strings.HasSuffix(t.PkgPath(), "/awk") {
		return true
	}
	switch t.Name() {
	case "Program", "Rule", "Block", "ExprStmt", "IfStmt", "ForStmt", "ForInStmt", "WhileStmt",
		"DoWhileStmt", "NextStmt", "ExitStmt", "BreakStmt", "ContinueStmt", "DeleteStmt",
		"ReturnStmt", "NumberLit", "StringLit", "ArrayRef", "BinaryExpr", "UnaryExpr",
		"TernaryExpr", "InExpr", "ConcatExpr", "MatchExpr", "ExprPattern", "BeginPattern",
		"EndPattern":
		return true
	}
	return false
}

// projectFields is field projection: a first compute stage that is an awk
// program reading only some fields (awkFieldReads) gets, from a shard that
// has the field form, just those fields' values instead of each whole line, so
// far fewer bytes cross to compute (QueryPlan.FieldSQL). Compute rebuilds the
// record, and the stage still runs, verbatim, and sees the same $N, $NF and NF.
// A shard without the form sends the whole lines. It needs the stage to read
// shard lines as text, with no other projection ahead of it, no pushed line
// count (the shard would apply it to rows, and the form can't wrap a tail), and
// a finite log source.
func (c *compiled) projectFields(limited bool) {
	const pass = "field-projection"
	if len(c.compute) == 0 || c.compute[0].cmd != "awk" || len(c.rowStages) > 0 || c.group != nil {
		return
	}
	stage := c.compute[0]
	label := c.label(stage.nodes[0])
	switch {
	case !c.d.Supports(dialect.ListIndex):
		c.logf(pass, "skipped %s: the store can't pick a field out of a line", label)
		return
	case limited:
		c.logf(pass, "skipped %s: a pushed line count is applied to rows, not the lines it reads", label)
		return
	case c.selected != nil:
		c.logf(pass, "skipped %s: a projection ahead of it already changed the text", label)
		return
	case c.source.follow || c.source.cmd != "cat":
		return
	}
	if stage.fields == nil {
		if stage.fieldsWhy != "" {
			c.logf(pass, "skipped %s: %s", label, stage.fieldsWhy)
		}
		return
	}
	reads := *stage.fields
	c.fieldSpec = &reads
	c.logf(pass, "%s reads only %s: the shard sends just those", label, describeFields(reads))
}

// describeFields lists what a stage reads as a user would write it.
func describeFields(r fieldReads) string {
	var out []string
	for _, p := range r.positions {
		out = append(out, fmt.Sprintf("$%d", p))
	}
	if r.last {
		out = append(out, "$NF")
	}
	if r.nf {
		out = append(out, "NF")
	}
	if len(out) == 0 {
		return "no fields"
	}
	return strings.Join(out, ", ")
}
