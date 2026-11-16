package planner

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

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
