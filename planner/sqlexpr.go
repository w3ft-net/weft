package planner

import (
	"strings"

	"github.com/w3ft-net/weft/backends/logs/dialect"
)

// sqlExpr is a SQL expression the planner has decided on and not yet spelled.
// The frontends build one; render spells it in a dialect, so the engine's
// syntax is written in one place and a plan's expressions can be read, and
// compared, as what they mean.
type sqlExpr interface{ isSQLExpr() }

type (
	// colRef is a column or other bare identifier.
	colRef string
	// strLit is a string constant, unquoted.
	strLit string
	// numLit is a number constant, as it is written.
	numLit string

	// listIdx is element n of a list; a negative n counts from the end.
	listIdx struct {
		list sqlExpr
		n    int
	}
	byteLen       struct{ x sqlExpr }
	castText      struct{ x sqlExpr }
	tryCastDouble struct{ x sqlExpr }
	isNull        struct{ x sqlExpr }
	notExpr       struct{ x sqlExpr }
	coalesce      []sqlExpr

	cmpExpr struct {
		op   dialect.CmpOp
		l, r sqlExpr
	}
	regexExpr struct {
		col        sqlExpr
		pattern    string
		ignoreCase bool
	}
	andExpr struct{ l, r sqlExpr }

	// typedChoice is base, or typed on a shard whose facts say every row holds a
	// number at locator ("$N" for awk field N, ".key" for a payload key): the
	// comparison is then the cast alone, with none of the cases for a value that
	// isn't a number. The planner stays pure: it emits what it can prove from the
	// text, and the coordinator picks per shard which form to run. typed never
	// drops a row base keeps and the stage would, so compute's stages still run
	// unchanged on what comes back.
	typedChoice struct {
		locator     string
		base, typed sqlExpr
	}
	// boolLit is true or false.
	boolLit bool
	// groupExpr is x in parentheses.
	groupExpr struct{ x sqlExpr }
	// appendExpr joins text with the concatenation operator.
	appendExpr []sqlExpr
	// caseExpr is a CASE: each when is a condition and its value.
	caseExpr struct {
		whens [][2]sqlExpr
		els   sqlExpr
	}

	// The JSON nodes read the payload column at a JSONPath.
	jsonExtract  struct{ path string }
	jsonAsNumber struct{ path string }
	jsonAsBool   struct{ path string }
	jsonAsString struct{ path string }
	jsonKindIs   struct {
		path  string
		kinds []dialect.JSONKind
	}
	// jsonIsObject is true where the whole payload is an object.
	jsonIsObject struct{}
	// jsonObject builds an object from alternating keys and values.
	jsonObject []sqlExpr
	// jsonOf is x as a JSON value.
	jsonOf struct{ x sqlExpr }
	// jsonText is the text jq prints for the value at path.
	jsonText struct {
		path string
		raw  bool
	}

	// concatExpr joins terms; concatWS joins them with sep between.
	concatExpr []sqlExpr
	concatWS   struct {
		sep   sqlExpr
		terms []sqlExpr
	}
	// splitExpr is x cut at sep, as a list.
	splitExpr struct{ x, sep sqlExpr }
	// listElement is the list of element n of list; listFrom the elements from n
	// on; listRange the elements lo to hi; listFlatten lists as one; listJoin the
	// elements with sep between.
	listElement struct {
		list sqlExpr
		n    int
	}
	listFrom struct {
		list sqlExpr
		n    int
	}
	listRange struct {
		list   sqlExpr
		lo, hi int
	}
	listFlatten []sqlExpr
	// listLen is the number of elements of list.
	listLen  struct{ list sqlExpr }
	listJoin struct{ list, sep sqlExpr }
	// orExpr is terms ORed, in parentheses. wrap parenthesizes each term that is
	// itself a compound, found by its AND or OR in the rendered text.
	orExpr struct {
		terms []sqlExpr
		wrap  bool
	}
)

func (colRef) isSQLExpr()        {}
func (strLit) isSQLExpr()        {}
func (numLit) isSQLExpr()        {}
func (listIdx) isSQLExpr()       {}
func (byteLen) isSQLExpr()       {}
func (castText) isSQLExpr()      {}
func (tryCastDouble) isSQLExpr() {}
func (isNull) isSQLExpr()        {}
func (notExpr) isSQLExpr()       {}
func (coalesce) isSQLExpr()      {}
func (cmpExpr) isSQLExpr()       {}
func (regexExpr) isSQLExpr()     {}
func (andExpr) isSQLExpr()       {}
func (typedChoice) isSQLExpr()   {}
func (boolLit) isSQLExpr()       {}
func (groupExpr) isSQLExpr()     {}
func (appendExpr) isSQLExpr()    {}
func (caseExpr) isSQLExpr()      {}
func (jsonExtract) isSQLExpr()   {}
func (jsonAsNumber) isSQLExpr()  {}
func (jsonAsBool) isSQLExpr()    {}
func (jsonAsString) isSQLExpr()  {}
func (jsonKindIs) isSQLExpr()    {}
func (jsonIsObject) isSQLExpr()  {}
func (jsonObject) isSQLExpr()    {}
func (jsonOf) isSQLExpr()        {}
func (jsonText) isSQLExpr()      {}
func (concatExpr) isSQLExpr()    {}
func (concatWS) isSQLExpr()      {}
func (splitExpr) isSQLExpr()     {}
func (listElement) isSQLExpr()   {}
func (listFrom) isSQLExpr()      {}
func (listRange) isSQLExpr()     {}
func (listFlatten) isSQLExpr()   {}
func (listLen) isSQLExpr()       {}
func (listJoin) isSQLExpr()      {}
func (orExpr) isSQLExpr()        {}

// renderIn spells e in d. A nil e is "": no expression. With typed, a typed
// choice is its typed form, the one for a shard whose facts hold.
func renderIn(e sqlExpr, d dialect.Dialect, typed bool) string {
	switch e := e.(type) {
	case nil:
		return ""
	case typedChoice:
		if typed {
			return renderIn(e.typed, d, typed)
		}
		return renderIn(e.base, d, typed)
	case colRef:
		return string(e)
	case strLit:
		return d.StringLit(string(e))
	case numLit:
		return string(e)
	case listIdx:
		return d.ListIndex(renderIn(e.list, d, typed), e.n)
	case byteLen:
		return d.ByteLen(renderIn(e.x, d, typed))
	case castText:
		return d.CastText(renderIn(e.x, d, typed))
	case tryCastDouble:
		return d.TryCastDouble(renderIn(e.x, d, typed))
	case isNull:
		return d.IsNull(renderIn(e.x, d, typed))
	case notExpr:
		return d.Not(renderIn(e.x, d, typed))
	case coalesce:
		return d.Coalesce(renderAll(e, d, typed)...)
	case cmpExpr:
		return d.Cmp(e.op, renderIn(e.l, d, typed), renderIn(e.r, d, typed))
	case regexExpr:
		return d.Regex(renderIn(e.col, d, typed), e.pattern, e.ignoreCase)
	case andExpr:
		return d.And(renderIn(e.l, d, typed), renderIn(e.r, d, typed))
	case boolLit:
		return d.BoolLit(bool(e))
	case groupExpr:
		return "(" + renderIn(e.x, d, typed) + ")"
	case appendExpr:
		return d.Append(renderAll(e, d, typed)...)
	case caseExpr:
		whens := make([][2]string, len(e.whens))
		for i, w := range e.whens {
			whens[i] = [2]string{renderIn(w[0], d, typed), renderIn(w[1], d, typed)}
		}
		return d.Case(whens, renderIn(e.els, d, typed))
	case jsonExtract:
		return d.JSONExtract(payloadCol, e.path)
	case jsonAsNumber:
		return d.JSONAsNumber(payloadCol, e.path)
	case jsonAsBool:
		return d.JSONAsBool(payloadCol, e.path)
	case jsonAsString:
		return d.JSONAsString(payloadCol, e.path)
	case jsonKindIs:
		return d.JSONKindIs(payloadCol, e.path, e.kinds...)
	case jsonIsObject:
		return d.JSONIsObject(payloadCol)
	case jsonObject:
		return d.JSONObject(renderAll(e, d, typed)...)
	case jsonOf:
		return d.JSONOf(renderIn(e.x, d, typed))
	case jsonText:
		return d.JSONText(payloadCol, e.path, e.raw)
	case concatExpr:
		return d.Concat(renderAll(e, d, typed)...)
	case concatWS:
		return d.ConcatWS(renderIn(e.sep, d, typed), renderAll(e.terms, d, typed)...)
	case splitExpr:
		return d.Split(renderIn(e.x, d, typed), renderIn(e.sep, d, typed))
	case listElement:
		return d.ListElement(renderIn(e.list, d, typed), e.n)
	case listFrom:
		return d.ListFrom(renderIn(e.list, d, typed), e.n)
	case listRange:
		return d.ListRange(renderIn(e.list, d, typed), e.lo, e.hi)
	case listFlatten:
		return d.ListFlatten(renderAll(e, d, typed)...)
	case listJoin:
		return d.ListJoin(renderIn(e.list, d, typed), renderIn(e.sep, d, typed))
	case listLen:
		return d.ListLen(renderIn(e.list, d, typed))
	case orExpr:
		out := ""
		for i, t := range e.terms {
			s := renderIn(t, d, typed)
			if e.wrap {
				s = parenthesizeIfCompound(s)
			}
			if i == 0 {
				out = s
				continue
			}
			out = d.Or(out, s)
		}
		return "(" + out + ")"
	}
	panic("planner: render: unhandled sqlExpr")
}

// render spells e in d, every typed choice in its base form.
func render(e sqlExpr, d dialect.Dialect) string { return renderIn(e, d, false) }

// payloadCol is the column the JSON nodes read.
const payloadCol = "payload"

// and is the expressions ANDed, left to right: nil for none.
func and(es ...sqlExpr) sqlExpr {
	var out sqlExpr
	for _, e := range es {
		if out == nil {
			out = e
			continue
		}
		out = andExpr{out, e}
	}
	return out
}

func renderAll(es []sqlExpr, d dialect.Dialect, typed bool) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = renderIn(e, d, typed)
	}
	return out
}

// children are the expressions e is built from. A typed choice has its base
// only: the typed form is read where the choice is.
func children(e sqlExpr) []sqlExpr {
	switch e := e.(type) {
	case listIdx:
		return []sqlExpr{e.list}
	case byteLen:
		return []sqlExpr{e.x}
	case castText:
		return []sqlExpr{e.x}
	case tryCastDouble:
		return []sqlExpr{e.x}
	case isNull:
		return []sqlExpr{e.x}
	case notExpr:
		return []sqlExpr{e.x}
	case groupExpr:
		return []sqlExpr{e.x}
	case jsonOf:
		return []sqlExpr{e.x}
	case listLen:
		return []sqlExpr{e.list}
	case listElement:
		return []sqlExpr{e.list}
	case listFrom:
		return []sqlExpr{e.list}
	case listRange:
		return []sqlExpr{e.list}
	case regexExpr:
		return []sqlExpr{e.col}
	case coalesce:
		return e
	case appendExpr:
		return e
	case concatExpr:
		return e
	case jsonObject:
		return e
	case listFlatten:
		return e
	case cmpExpr:
		return []sqlExpr{e.l, e.r}
	case andExpr:
		return []sqlExpr{e.l, e.r}
	case splitExpr:
		return []sqlExpr{e.x, e.sep}
	case listJoin:
		return []sqlExpr{e.list, e.sep}
	case concatWS:
		return append([]sqlExpr{e.sep}, e.terms...)
	case orExpr:
		return e.terms
	case caseExpr:
		var out []sqlExpr
		for _, w := range e.whens {
			out = append(out, w[0], w[1])
		}
		return append(out, e.els)
	case typedChoice:
		return []sqlExpr{e.base}
	}
	return nil
}

// walk calls visit on e and everything it is built from, parents first.
func walk(e sqlExpr, visit func(sqlExpr)) {
	if e == nil {
		return
	}
	visit(e)
	for _, c := range children(e) {
		walk(c, visit)
	}
}

// features are the dialect features e needs, in its base form.
func features(e sqlExpr) []dialect.Feature {
	var fs []dialect.Feature
	walk(e, func(e sqlExpr) {
		switch e.(type) {
		case listIdx:
			fs = append(fs, dialect.ListIndex)
		case byteLen:
			fs = append(fs, dialect.ByteLen)
		case tryCastDouble:
			fs = append(fs, dialect.TryCast)
		case regexExpr:
			fs = append(fs, dialect.Regex)
		case splitExpr:
			fs = append(fs, dialect.Split)
		case listElement, listFrom, listRange, listLen, listFlatten, listJoin:
			fs = append(fs, dialect.Lists)
		case jsonExtract, jsonAsNumber, jsonAsBool, jsonAsString, jsonKindIs, jsonIsObject, jsonText, jsonObject, jsonOf:
			fs = append(fs, dialect.JSON)
		}
	})
	return fs
}

// typedChoices are the typed choices in e.
func typedChoices(e sqlExpr) []typedChoice {
	var out []typedChoice
	walk(e, func(e sqlExpr) {
		if c, ok := e.(typedChoice); ok {
			out = append(out, c)
		}
	})
	return out
}

// supported reports whether d has every feature e needs.
func supported(e sqlExpr, d dialect.Dialect) bool {
	for _, f := range features(e) {
		if !d.Supports(f) {
			return false
		}
	}
	return true
}

// parenthesizeIfCompound wraps a rendered predicate in parens if it holds an AND
// or an OR: found by the text, so a term that is already parenthesized is
// wrapped again, which the pinned SQL records.
func parenthesizeIfCompound(pred string) string {
	if strings.Contains(pred, " AND ") || strings.Contains(pred, " OR ") {
		return "(" + pred + ")"
	}
	return pred
}
