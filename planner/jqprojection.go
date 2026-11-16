package planner

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/graybeardcorp/warp/pkg/jq"
)

// jqKeyName is a payload key a narrowed payload can name in a JSON path
// without quoting.
var jqKeyName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// jqRootKeys is which top-level keys of a row's payload a jq program reads,
// when it reads nothing else of the object: every use of the payload as a
// whole (a bare `.`, keys, length, has, `.[]`, `..`, map, to_entries, ...)
// is refused, since an object holding only some keys would answer them
// differently. why says what was refused.
//
// The program is followed through the value `.` holds. At the root, `.a` takes
// a key and leaves the root behind: whatever follows reads that key's value,
// whole, so it can do anything with it. Select, comma, if and the operators
// keep the root as their input. So only a few node types are allowed to see the
// root, and every other node is refused there; away from the root any node is
// fine (except one that reads other input), since nothing reachable from a
// key's value leads back to the rest of the object.
func jqRootKeys(expr jq.Expr) (keys []string, why string) {
	w := &rootWalk{keys: map[string]bool{}}
	if rootOut, ok := w.walk(expr, true); !ok {
		return nil, w.why
	} else if rootOut {
		return nil, "it outputs the whole object"
	}
	for k := range w.keys {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, ""
}

type rootWalk struct {
	keys map[string]bool
	why  string
}

func (w *rootWalk) refuse(why string) (bool, bool) {
	if w.why == "" {
		w.why = why
	}
	return false, false
}

// value walks an operand that is used as a value: the root itself must not be
// it.
func (w *rootWalk) value(e jq.Expr, atRoot bool) bool {
	rootOut, ok := w.walk(e, atRoot)
	if ok && rootOut {
		w.refuse("it uses the whole object as a value")
		return false
	}
	return ok
}

// walk follows expr evaluated on an input that is the whole payload (atRoot)
// or something derived from a key's value. ok is false when the program reads
// more of the payload than named keys; rootOut is whether the expression can
// output the root itself.
func (w *rootWalk) walk(expr jq.Expr, atRoot bool) (rootOut, ok bool) {
	if !atRoot {
		if jqContains(expr, func(n any) bool { _, in := n.(jq.InputExpr); return in }) {
			return w.refuse("it reads other input")
		}
		return false, true
	}
	switch e := expr.(type) {
	case jq.IdentityExpr:
		return true, true
	case jq.FieldExpr:
		return w.key(e.Name)
	case jq.IndexExpr:
		lit, isLit := e.Index.(jq.LiteralExpr)
		if !isLit || lit.Value.Type != jq.TypeString {
			return w.refuse("it indexes the whole object by something other than a key")
		}
		return w.key(lit.Value.Str)
	case jq.PipeExpr:
		leftRoot, ok := w.walk(e.Left, true)
		if !ok {
			return false, false
		}
		return w.walk(e.Right, leftRoot)
	case jq.SelectExpr:
		if !w.value(e.Cond, true) {
			return false, false
		}
		return true, true
	case jq.OptionalExpr:
		return w.walk(e.Body, true)
	case jq.CommaExpr:
		l, ok := w.walk(e.Left, true)
		if !ok {
			return false, false
		}
		r, ok := w.walk(e.Right, true)
		return l || r, ok
	case jq.AlternativeExpr:
		l, ok := w.walk(e.Left, true)
		if !ok {
			return false, false
		}
		r, ok := w.walk(e.Right, true)
		return l || r, ok
	case jq.IfExpr:
		if !w.value(e.Cond, true) {
			return false, false
		}
		var out bool
		for _, branch := range []jq.Expr{e.Then, e.Else} {
			if branch == nil {
				out = true // no else leaves the input as it was
				continue
			}
			r, ok := w.walk(branch, true)
			if !ok {
				return false, false
			}
			out = out || r
		}
		return out, true
	case jq.AddExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.SubExprAST:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.MulExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.DivExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.ModExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.EqExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.NeExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.LtExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.GtExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.LeExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.GeExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.AndExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.OrExpr:
		return false, w.value(e.Left, true) && w.value(e.Right, true)
	case jq.NotExpr:
		return false, w.value(e.Operand, true)
	case jq.NegExpr:
		return false, w.value(e.Operand, true)
	case jq.StringInterp:
		for _, part := range e.Parts {
			if !w.value(part, true) {
				return false, false
			}
		}
		return false, true
	case jq.ArrayExpr:
		if e.Elems == nil {
			return false, true
		}
		return false, w.value(e.Elems, true)
	case jq.ObjectExpr:
		for _, p := range e.Pairs {
			if !w.value(p.Key, true) || !w.value(p.Value, true) {
				return false, false
			}
		}
		return false, true
	case jq.LiteralExpr, jq.VarRef, jq.EmptyExpr:
		return false, true
	}
	return w.refuse(fmt.Sprintf("%s reads the whole object", jqNodeName(expr)))
}

// key records a top-level key the program reads.
func (w *rootWalk) key(name string) (bool, bool) {
	if !jqKeyName.MatchString(name) {
		return w.refuse("a key with characters it can't name in a path")
	}
	w.keys[name] = true
	return false, true
}

// jqNodeName is a jq syntax node's type, as a word.
func jqNodeName(e jq.Expr) string {
	return strings.TrimSuffix(reflect.TypeOf(e).Name(), "Expr")
}

// jqContains reports whether match holds for any node under root.
func jqContains(root any, match func(any) bool) bool {
	var walk func(v reflect.Value) bool
	walk = func(v reflect.Value) bool {
		switch v.Kind() {
		case reflect.Interface, reflect.Pointer:
			if v.IsNil() {
				return false
			}
			return walk(v.Elem())
		case reflect.Struct:
			if v.CanInterface() && match(v.Interface()) {
				return true
			}
			for i := 0; i < v.NumField(); i++ {
				if walk(v.Field(i)) {
					return true
				}
			}
		case reflect.Slice:
			for i := 0; i < v.Len(); i++ {
				if walk(v.Index(i)) {
					return true
				}
			}
		}
		return false
	}
	return walk(reflect.ValueOf(root))
}

// narrowedPayloadSQL is the SQL for a payload holding only the given
// top-level keys, each with its value whole (a missing key is null, which
// reads the same). A payload is sent as it is when narrowing it could change
// what jq reads: one that isn't an object (jq's answer to indexing it is its
// own), and one holding a bare integer -0 (a number token: after a colon,
// bracket or comma, before a comma, bracket or brace), which the JSON functions
// would write back as 0. A string that merely looks like one costs only the
// bytes.
func narrowedPayloadSQL(keys []string) string {
	const whole = "CAST(payload AS VARCHAR)"
	const narrowable = `json_type(payload) = 'OBJECT' AND NOT regexp_matches(` + whole + `, '[:\[,]\s*-0\s*[,\]}]')`
	object := "'{}'"
	if len(keys) > 0 {
		pairs := make([]string, len(keys))
		for i, k := range keys {
			pairs[i] = fmt.Sprintf("'%s', json_extract(payload, '$.%s')", k, k)
		}
		object = fmt.Sprintf("CAST(json_object(%s) AS VARCHAR)", strings.Join(pairs, ", "))
	}
	return fmt.Sprintf("CASE WHEN %s THEN %s ELSE %s END", narrowable, object, whole)
}

// projectPayload is field projection for a first jq stage that reads the
// payload: when the program reads only some top-level keys (jqRootKeys), the
// shard sends an object with just those instead of the whole payload. The
// stage runs, verbatim, on the same values.
func (c *compiled) projectPayload() {
	const pass = "field-projection"
	if len(c.compute) == 0 || c.compute[0].cmd != "jq" || !c.readsPayload || len(c.rowStages) > 0 || c.group != nil {
		return
	}
	stage := c.compute[0]
	label := c.label(stage.nodes[0])
	if c.source.follow || c.source.cmd == "trace" || c.source.cmd == "trace_spans" || c.source.cmd == "trace_list" {
		return
	}
	p, _ := analyzeJq(stage.args)
	if p == nil || p.expr == nil {
		return
	}
	if p.slurp {
		c.logf(pass, "skipped %s: it reads every row's payload at once", label)
		return
	}
	keys, refused := jqRootKeys(p.expr)
	if refused != "" {
		c.logf(pass, "skipped %s: %s", label, refused)
		return
	}
	c.payloadSelect = narrowedPayloadSQL(keys)
	if len(keys) == 0 {
		c.logf(pass, "%s reads no payload keys: the shard sends none", label)
		return
	}
	c.logf(pass, "%s reads only .%s: the shard sends just those", label, strings.Join(keys, ", ."))
}
