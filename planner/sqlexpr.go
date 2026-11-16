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

// render spells e in d. A nil e is "": no expression.
func render(e sqlExpr, d dialect.Dialect) string {
	switch e := e.(type) {
	case nil:
		return ""
	case colRef:
		return string(e)
	case strLit:
		return d.StringLit(string(e))
	case numLit:
		return string(e)
	case listIdx:
		return d.ListIndex(render(e.list, d), e.n)
	case byteLen:
		return d.ByteLen(render(e.x, d))
	case castText:
		return d.CastText(render(e.x, d))
	case tryCastDouble:
		return d.TryCastDouble(render(e.x, d))
	case isNull:
		return d.IsNull(render(e.x, d))
	case notExpr:
		return d.Not(render(e.x, d))
	case coalesce:
		return d.Coalesce(renderAll(e, d)...)
	case cmpExpr:
		return d.Cmp(e.op, render(e.l, d), render(e.r, d))
	case regexExpr:
		return d.Regex(render(e.col, d), e.pattern, e.ignoreCase)
	case andExpr:
		return d.And(render(e.l, d), render(e.r, d))
	case boolLit:
		return d.BoolLit(bool(e))
	case groupExpr:
		return "(" + render(e.x, d) + ")"
	case appendExpr:
		return d.Append(renderAll(e, d)...)
	case caseExpr:
		whens := make([][2]string, len(e.whens))
		for i, w := range e.whens {
			whens[i] = [2]string{render(w[0], d), render(w[1], d)}
		}
		return d.Case(whens, render(e.els, d))
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
		return d.JSONObject(renderAll(e, d)...)
	case jsonOf:
		return d.JSONOf(render(e.x, d))
	case jsonText:
		return d.JSONText(payloadCol, e.path, e.raw)
	case concatExpr:
		return d.Concat(renderAll(e, d)...)
	case concatWS:
		return d.ConcatWS(render(e.sep, d), renderAll(e.terms, d)...)
	case splitExpr:
		return d.Split(render(e.x, d), render(e.sep, d))
	case listElement:
		return d.ListElement(render(e.list, d), e.n)
	case listFrom:
		return d.ListFrom(render(e.list, d), e.n)
	case listRange:
		return d.ListRange(render(e.list, d), e.lo, e.hi)
	case listFlatten:
		return d.ListFlatten(renderAll(e, d)...)
	case listJoin:
		return d.ListJoin(render(e.list, d), render(e.sep, d))
	case listLen:
		return d.ListLen(render(e.list, d))
	case orExpr:
		out := ""
		for i, t := range e.terms {
			s := render(t, d)
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

func renderAll(es []sqlExpr, d dialect.Dialect) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = render(e, d)
	}
	return out
}

// features are the dialect features e needs.
func features(e sqlExpr) []dialect.Feature {
	var fs []dialect.Feature
	var walk func(sqlExpr)
	walk = func(e sqlExpr) {
		switch e := e.(type) {
		case listIdx:
			fs = append(fs, dialect.ListIndex)
			walk(e.list)
		case byteLen:
			fs = append(fs, dialect.ByteLen)
			walk(e.x)
		case tryCastDouble:
			fs = append(fs, dialect.TryCast)
			walk(e.x)
		case regexExpr:
			fs = append(fs, dialect.Regex)
			walk(e.col)
		case castText:
			walk(e.x)
		case groupExpr:
			walk(e.x)
		case appendExpr:
			for _, t := range e {
				walk(t)
			}
		case caseExpr:
			for _, w := range e.whens {
				walk(w[0])
				walk(w[1])
			}
			walk(e.els)
		case jsonExtract, jsonAsNumber, jsonAsBool, jsonAsString, jsonKindIs, jsonIsObject, jsonText:
			fs = append(fs, dialect.JSON)
		case jsonObject:
			fs = append(fs, dialect.JSON)
			for _, t := range e {
				walk(t)
			}
		case jsonOf:
			fs = append(fs, dialect.JSON)
			walk(e.x)
		case concatExpr:
			for _, t := range e {
				walk(t)
			}
		case concatWS:
			walk(e.sep)
			for _, t := range e.terms {
				walk(t)
			}
		case splitExpr:
			fs = append(fs, dialect.Split)
			walk(e.x)
			walk(e.sep)
		case listElement:
			fs = append(fs, dialect.Lists)
			walk(e.list)
		case listFrom:
			fs = append(fs, dialect.Lists)
			walk(e.list)
		case listRange:
			fs = append(fs, dialect.Lists)
			walk(e.list)
		case listLen:
			fs = append(fs, dialect.Lists)
			walk(e.list)
		case listFlatten:
			fs = append(fs, dialect.Lists)
			for _, l := range e {
				walk(l)
			}
		case listJoin:
			fs = append(fs, dialect.Lists)
			walk(e.list)
			walk(e.sep)
		case isNull:
			walk(e.x)
		case notExpr:
			walk(e.x)
		case coalesce:
			for _, a := range e {
				walk(a)
			}
		case cmpExpr:
			walk(e.l)
			walk(e.r)
		case andExpr:
			walk(e.l)
			walk(e.r)
		case orExpr:
			for _, t := range e.terms {
				walk(t)
			}
		}
	}
	walk(e)
	return fs
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
