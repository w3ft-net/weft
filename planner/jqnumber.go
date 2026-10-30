package planner

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// jqNumberTypes are the JSON types DuckDB reports for a number.
const jqNumberTypes = "'BIGINT', 'UBIGINT', 'DOUBLE', 'HUGEINT'"

// jqLeafSQL is the SQL for the text jq prints for the value at a payload path:
// compact JSON, "null" for a missing value. A number is rewritten the way jq
// prints it (jqNumberSQL), since the JSON functions keep the payload's own
// spelling (2.0, 1e3, a 20-digit integer) and jq prints the float64 it parses.
// Anything else is the JSON as it is.
func jqLeafSQL(path jqPath) string {
	p := path.jsonPath()
	extract := fmt.Sprintf("json_extract(payload, %s)", p)
	typ := fmt.Sprintf("json_type(payload, %s)", p)
	text := fmt.Sprintf("CAST(%s AS VARCHAR)", extract)
	x := fmt.Sprintf("CAST(%s AS DOUBLE)", extract)
	// Most numbers are already written as jq writes them, and the formatter
	// below is long: an integer a float64 holds exactly is its digits, and a
	// DOUBLE that DuckDB writes in plain decimal (no exponent: between 1e-4 and
	// 1e15) has jq's digits too, without a trailing ".0".
	return fmt.Sprintf("COALESCE(CASE"+
		" WHEN %[1]s IN ('BIGINT', 'UBIGINT') AND abs(%[2]s) < 9007199254740992 THEN %[3]s"+
		" WHEN %[1]s = 'DOUBLE' AND abs(%[2]s) BETWEEN 0.0001 AND 1e15 AND %[3]s NOT LIKE '%%e%%'"+
		" THEN CASE WHEN %[3]s LIKE '%%.0' THEN substr(%[3]s, 1, length(%[3]s) - 2) ELSE %[3]s END"+
		" WHEN %[1]s IN (%[4]s) THEN %[5]s"+
		" WHEN %[1]s = 'VARCHAR' THEN %[6]s"+
		" ELSE %[3]s END, 'null')", typ, x, text, jqNumberTypes, jqNumberSQL(x), jqStringSQL(text))
}

// jqStringSQL is a JSON string's text as warp's jq prints it. The JSON
// functions write a string with only the escapes JSON needs. Warp writes a
// string bare when every byte is printable ASCII and not a quote or backslash
// (jqStringJSON), and otherwise through Go's encoder, which also escapes <, >,
// & and the line separators U+2028 and U+2029. In the JSON text a string needs
// that when it holds a backslash (the quote, backslash or a control character)
// or a character beyond ASCII.
func jqStringSQL(text string) string {
	// The JSON functions write backspace and form feed as \b and \f; Go's
	// encoder writes \u0008 and \u000c. An escaped backslash (\\) followed by b or
	// f is not one of those, so such pairs are set aside while it is done: chr(1)
	// can't appear in the text, since it is written \u0001.
	escaped := text
	for _, r := range [][2]string{
		{`'\\'`, "chr(1)"}, {`'\b'`, `'\u0008'`}, {`'\f'`, `'\u000c'`}, {"chr(1)", `'\\'`},
		{"'<'", `'\u003c'`}, {"'>'", `'\u003e'`}, {"'&'", `'\u0026'`}, {"chr(8232)", `'\u2028'`}, {"chr(8233)", `'\u2029'`},
	} {
		escaped = fmt.Sprintf("replace(%s, %s, %s)", escaped, r[0], r[1])
	}
	return fmt.Sprintf(`CASE WHEN regexp_matches(%s, '[\\\x7f-\x{10ffff}]') THEN %s ELSE %s END`, text, escaped, text)
}

// jqNumberSQL is the SQL text jq prints for a finite DOUBLE expression x that
// isn't one of the plain cases jqLeafSQL handles itself. jq writes the shortest
// decimal digits that round-trip the double, in decimal notation unless the
// decimal point falls four or more places left of the first digit, or more
// than 15 places right of the last significant one, and then as d.ddde+XX (at
// least two exponent digits). Zero keeps its sign.
//
// DuckDB's text for a DOUBLE has the same shortest digits. Below 1e-4 it is
// already jq's (1e-07, 1.234e-05, 5e-324), and so is an exponent form jq also
// writes as an exponent (1e+21, 1.7976931348623157e+308). What differs is a
// large number jq writes out in full (12345678901234567000), so those digits
// are laid out again from DuckDB's mantissa and exponent.
func jqNumberSQL(x string) string {
	t := fmt.Sprintf("CAST(abs(%s) AS VARCHAR)", x)
	digits := fmt.Sprintf("replace(split_part(%s, 'e', 1), '.', '')", t)
	n := fmt.Sprintf("length(%s)", digits)
	decpt := fmt.Sprintf("(CAST(split_part(%s, 'e', 2) AS INTEGER) + 1)", t)
	large := fmt.Sprintf("CASE WHEN %[1]s NOT LIKE '%%e%%' THEN CASE WHEN %[1]s LIKE '%%.0' THEN substr(%[1]s, 1, length(%[1]s) - 2) ELSE %[1]s END"+
		" WHEN %[4]s > %[3]s + 15 THEN %[1]s"+
		" WHEN %[4]s >= %[3]s THEN %[2]s || repeat('0', %[4]s - %[3]s)"+
		" ELSE substr(%[2]s, 1, %[4]s) || '.' || substr(%[2]s, %[4]s + 1) END",
		t, digits, n, decpt)
	body := fmt.Sprintf("CASE WHEN abs(%s) < 0.0001 THEN %s ELSE %s END", x, t, large)
	zero := fmt.Sprintf("CASE WHEN CAST(%s AS VARCHAR) LIKE '-%%' THEN '-0' ELSE '0' END", x)
	return fmt.Sprintf("CASE WHEN %[1]s = 0 THEN %[2]s WHEN %[1]s < 0 THEN '-' || %[3]s ELSE %[3]s END", x, zero, body)
}

// jqNumberText is the text jq prints for a number, as warp's jq writes it: the
// shortest digits that round-trip the float64, laid out as jq's dtoa does.
func jqNumberText(n float64) string {
	switch {
	case math.IsNaN(n):
		return "null"
	case math.IsInf(n, 1):
		return "1.7976931348623157e+308"
	case math.IsInf(n, -1):
		return "-1.7976931348623157e+308"
	case n == 0:
		if math.Signbit(n) {
			return "-0"
		}
		return "0"
	}
	sign := ""
	if n < 0 {
		sign, n = "-", -n
	}
	mantissa, expStr, _ := strings.Cut(strconv.FormatFloat(n, 'e', -1, 64), "e")
	exp, _ := strconv.Atoi(expStr)
	digits := strings.TrimRight(strings.Replace(mantissa, ".", "", 1), "0")
	nsig, decpt := len(digits), exp+1
	switch {
	case decpt <= -4 || decpt > nsig+15:
		out := digits[:1]
		if nsig > 1 {
			out += "." + digits[1:]
		}
		e := decpt - 1
		esign := "+"
		if e < 0 {
			esign, e = "-", -e
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, esign, e)
	case decpt <= 0:
		return sign + "0." + strings.Repeat("0", -decpt) + digits
	case decpt >= nsig:
		return sign + digits + strings.Repeat("0", decpt-nsig)
	}
	return sign + digits[:decpt] + "." + digits[decpt:]
}

// jqStringJSON is a string as warp's jq prints it: bare between quotes when
// every byte is printable ASCII and not a quote or backslash, else as Go's JSON
// encoder writes it (which also escapes <, > and &).
func jqStringJSON(s string) string {
	plain := true
	for i := 0; i < len(s); i++ {
		if c := s[i]; c < 0x20 || c == '"' || c == '\\' || c > 0x7e {
			plain = false
			break
		}
	}
	if plain {
		return `"` + s + `"`
	}
	b, _ := json.Marshal(s)
	return string(b)
}
