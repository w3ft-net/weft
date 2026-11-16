// Package dialect is how the planner spells SQL for a store: everything that
// can differ between engines. The planner decides what to ask; a Dialect says
// how it is written. DuckDB is the one the open build ships. See
// docs/plans/pluggable-log-storage.md.
//
// A method takes operands already rendered and returns SQL text. A dialect
// that lacks a Feature still implements its methods, but the planner never
// calls them: it leaves what needs the Feature to compute. Which operand
// needs parentheses is the planner's call, not the dialect's. A method is added
// as the planner code that wrote that SQL by hand moves behind it.
package dialect

import (
	"fmt"
	"strconv"
	"strings"
)

// CmpOp is a comparison, by what it means rather than how an engine writes it.
type CmpOp string

const (
	EQ CmpOp = "="
	NE CmpOp = "!="
	LT CmpOp = "<"
	LE CmpOp = "<="
	GT CmpOp = ">"
	GE CmpOp = ">="
)

// Feature is something an engine may not have. The planner asks Supports before
// it pushes a stage that needs one, so the stage runs in compute instead.
type Feature int

const (
	// Regex is matching a pattern anywhere in a text value.
	Regex Feature = iota + 1
	// ListIndex is the Nth element of a list column.
	ListIndex
	// TryCast is a cast to a number that yields NULL instead of failing.
	TryCast
	// ByteLen is the length of a text value in bytes.
	ByteLen
	// Split is cutting a text value into a list at a separator.
	Split
	// Lists is picking from, joining and flattening lists of text.
	Lists

	// The features below are whole kinds of query. The SQL for them is still
	// written in the planner, as DuckDB text; a dialect says whether it can run
	// that text. It moves behind the dialect when a second engine needs it.

	// JSON is reading a JSON payload column: jq filters and projections, and
	// narrowing the payload a stage reads.
	JSON
	// Lines is splitting each row's text into one row per line, so a filter
	// can keep lines.
	Lines
	// Aggregates is grouping rows in the store: counts of lines, keyed awk
	// aggregates.
	Aggregates
	// Spans is the trace and span queries.
	Spans
)

// JSONKind is a kind of JSON value, in jq's order: null sorts below false and
// true, those below numbers, numbers below strings, then arrays, then objects.
type JSONKind int

const (
	KindNull JSONKind = iota
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

// ParamType is the type a bound parameter is cast to. Parameters travel as
// text, so a typed column is compared with the parameter cast to its type.
type ParamType int

const (
	ParamText ParamType = iota
	ParamInt64
)

// Dialect spells the SQL that differs between engines.
type Dialect interface {
	// Supports reports whether the engine has f.
	Supports(f Feature) bool

	// StringLit is s as a string literal.
	StringLit(s string) string
	// And, Or and Not combine boolean operands.
	And(l, r string) string
	Or(l, r string) string
	Not(x string) string
	// Cmp compares two operands.
	Cmp(op CmpOp, l, r string) string
	IsNull(x string) string
	Coalesce(args ...string) string
	// CastText is x as text.
	CastText(x string) string
	// TimestampParam is a `?` parameter cast to a timestamp, the type of ts.
	TimestampParam() string
	// TypedParam is a `?` parameter cast to t.
	TypedParam(t ParamType) string
	// CastInt64 is x as a 64-bit integer; TryCastInt64 yields NULL where it isn't
	// one.
	CastInt64(x string) string
	TryCastInt64(x string) string
	// EpochNs is a timestamp as nanoseconds since the Unix epoch.
	EpochNs(x string) string
	// FirstBy is, in a group, the value of x on the first row in order, where
	// order is an ORDER BY clause.
	FirstBy(x, order string) string
	// BoolLit is the constant true or false.
	BoolLit(b bool) string
	// Append joins text with the engine's concatenation operator; a NULL
	// operand makes the result NULL.
	Append(args ...string) string
	// Case is a CASE expression: each when is a condition then its value, in
	// order, and els the value where none holds.
	Case(whens [][2]string, els string) string
	// Concat joins its operands, a NULL as empty.
	Concat(args ...string) string
	// ConcatWS joins its operands with sep between them, a NULL skipped.
	ConcatWS(sep string, args ...string) string

	// The Features, each spelled by the engine that has it.

	// Regex is true where col, a text expression, holds a match for pattern
	// anywhere in it: the substring/find form, as grep means it, not a
	// whole-string match. pattern is RE2 syntax, unquoted; ignoreCase matches
	// without regard to case.
	Regex(col, pattern string, ignoreCase bool) string
	// ListIndex is element n of list. A negative n counts from the end.
	ListIndex(list string, n int) string
	// TryCastDouble is x as a DOUBLE, or NULL where it isn't one.
	TryCastDouble(x string) string
	// ByteLen is the byte length of the text x.
	ByteLen(x string) string

	// Split is x cut at each sep, as a list. A value without sep is a
	// list of one.
	Split(x, sep string) string
	// ListElement is the list holding only element n (counting from 1) of list,
	// which is empty where list has none.
	ListElement(list string, n int) string
	// ListFrom is the elements of list from n (counting from 1) on.
	ListFrom(list string, n int) string
	// ListRange is the elements of list from lo to hi, both counting from 1 and
	// inclusive; empty where lo is past hi.
	ListRange(list string, lo, hi int) string
	// ListFlatten is the lists, one after another, as one.
	ListFlatten(lists ...string) string
	// ListJoin is the elements of list with sep between them. A NULL element is
	// skipped, and a list of nothing but NULLs joins to NULL.
	ListJoin(list, sep string) string
	// ListLen is the number of elements of list.
	ListLen(list string) string
	// SplitLines is one row per line of the text x, in a SELECT list.
	SplitLines(x string) string
	// NumericSortKey is the number sort -n compares the text col by, as warp reads
	// it: the leading sign, digits and at most one dot after any spaces and
	// tabs; 0 where there is no number in them or it doesn't fit a float64. -0
	// is 0, which sort -n treats as equal to it.
	NumericSortKey(col string) string

	// The JSON methods read a JSON column, col. A path is JSONPath, unquoted
	// ("$.a.b[0]"); the engine spells it as it needs.

	// JSONExtract is the JSON value at path, as the engine's JSON type.
	JSONExtract(col, path string) string
	// JSONAsNumber, JSONAsBool and JSONAsString are the value at path as a
	// DOUBLE, a boolean and unescaped text; NULL where it is another type.
	JSONAsNumber(col, path string) string
	JSONAsBool(col, path string) string
	JSONAsString(col, path string) string
	// JSONKindIs is true where the value at path is one of kinds. A missing
	// value is KindNull.
	JSONKindIs(col, path string, kinds ...JSONKind) string
	// JSONIsObject is true where the whole of col is an object.
	JSONIsObject(col string) string
	// JSONObject builds an object from alternating keys and values.
	JSONObject(kv ...string) string
	// JSONOf is x as a JSON value.
	JSONOf(x string) string
	// RenderSpans is a trace or span aggregate over the logs table.
	RenderSpans(q SpanQuery) string
	// JSONText is the text jq prints for the value at path: compact JSON, "null"
	// for a missing value, a number as jq writes it, and a string escaped as
	// warp's jq escapes it. With raw (jq -r) a string prints unescaped. How
	// the engine writes a number, and which strings it escapes, is the engine's
	// to match, so this is a whole fragment.
	JSONText(col, path string, raw bool) string
}

// DuckDB is the dialect of the open build's stores.
var DuckDB Dialect = duckDB{}

type duckDB struct{}

func (duckDB) Supports(Feature) bool { return true }

func (duckDB) StringLit(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
func (duckDB) And(l, r string) string    { return l + " AND " + r }
func (duckDB) Or(l, r string) string     { return l + " OR " + r }
func (duckDB) Not(x string) string       { return "NOT " + x }
func (duckDB) IsNull(x string) string    { return x + " IS NULL" }
func (duckDB) CastText(x string) string  { return "CAST(" + x + " AS VARCHAR)" }
func (duckDB) BoolLit(b bool) string     { return strconv.FormatBool(b) }
func (duckDB) Append(args ...string) string {
	return strings.Join(args, " || ")
}
func (duckDB) Case(whens [][2]string, els string) string {
	var b strings.Builder
	b.WriteString("CASE")
	for _, w := range whens {
		b.WriteString(" WHEN " + w[0] + " THEN " + w[1])
	}
	return b.String() + " ELSE " + els + " END"
}
func (duckDB) Concat(args ...string) string {
	return "concat(" + strings.Join(args, ", ") + ")"
}
func (duckDB) ConcatWS(sep string, args ...string) string {
	return "concat_ws(" + sep + ", " + strings.Join(args, ", ") + ")"
}

func (duckDB) Cmp(op CmpOp, l, r string) string { return l + " " + string(op) + " " + r }

func (duckDB) Coalesce(args ...string) string {
	return "COALESCE(" + strings.Join(args, ", ") + ")"
}

// Regex is regexp_matches, DuckDB's substring/find form. Its `~` operator is a
// whole-string match, which returns nothing for the patterns users write.
func (d duckDB) Regex(col, pattern string, ignoreCase bool) string {
	if ignoreCase {
		return fmt.Sprintf("regexp_matches(%s, %s, 'i')", col, d.StringLit(pattern))
	}
	return fmt.Sprintf("regexp_matches(%s, %s)", col, d.StringLit(pattern))
}

func (duckDB) ListIndex(list string, n int) string {
	return fmt.Sprintf("list_extract(%s, %d)", list, n)
}
func (duckDB) TryCastDouble(x string) string { return "TRY_CAST(" + x + " AS DOUBLE)" }
func (duckDB) ByteLen(x string) string       { return "strlen(" + x + ")" }

func (duckDB) Split(x, sep string) string { return "string_split(" + x + ", " + sep + ")" }
func (duckDB) ListElement(list string, n int) string {
	return fmt.Sprintf("[%s[%d]]", list, n)
}
func (duckDB) ListFrom(list string, n int) string { return fmt.Sprintf("%s[%d:]", list, n) }
func (duckDB) ListRange(list string, lo, hi int) string {
	return fmt.Sprintf("%s[%d:%d]", list, lo, hi)
}
func (duckDB) ListFlatten(lists ...string) string {
	return "flatten([" + strings.Join(lists, ", ") + "])"
}
func (duckDB) ListJoin(list, sep string) string { return "array_to_string(" + list + ", " + sep + ")" }

func (duckDB) TimestampParam() string { return "CAST(? AS TIMESTAMP)" }

func (duckDB) TypedParam(t ParamType) string {
	if t == ParamInt64 {
		return "CAST(? AS BIGINT)"
	}
	return "CAST(? AS VARCHAR)"
}

func (duckDB) CastInt64(x string) string    { return "CAST(" + x + " AS BIGINT)" }
func (duckDB) TryCastInt64(x string) string { return "TRY_CAST(" + x + " AS BIGINT)" }
func (duckDB) EpochNs(x string) string      { return "epoch_ns(" + x + ")" }
func (duckDB) FirstBy(x, order string) string {
	return "first(" + x + " " + order + ")"
}
func (duckDB) ListLen(list string) string { return "len(" + list + ")" }
func (duckDB) SplitLines(x string) string { return "unnest(string_split(" + x + ", chr(10)))" }

func (duckDB) NumericSortKey(col string) string {
	k := `TRY_CAST(regexp_extract(` + col + `, '^[ \t]*([+-]?[0-9]*[.]?[0-9]*)', 1) AS DOUBLE)`
	return `CASE WHEN isfinite(` + k + `) THEN ` + k + ` + 0.0 ELSE 0.0 END`
}
