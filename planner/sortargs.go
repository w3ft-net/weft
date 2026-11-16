package planner

import (
	"strconv"
	"strings"
)

// sortArgs is what the planner needs to know of a `sort` stage's arguments, read
// the way warp's sort reads them (cmdSort), and only the forms it reads without
// complaint and without a surprise: -r, -n, clusters of them, and -k with the
// key as its own argument or attached. Anything else (-u, -t, an operand that
// names a file, a key it cannot parse) is not plain.
type sortArgs struct {
	plain bool
	// order is set when the stage orders `key value` lines by the number that
	// leads their second field: exactly one key starting at field 2 and ending
	// there or at the end of the line, compared numerically.
	order *ValueOrder
}

type sortKey struct {
	start, end int // 1-based fields; end 0 means to the end of the line
	numeric    bool
	reverse    bool
	modified   bool // has its own n/r, so the global -n/-r don't apply
}

// parseSortKey is warp's: N[,M], each with optional n and r modifiers.
func parseSortKey(spec string) (sortKey, bool) {
	var k sortKey
	parts := strings.Split(spec, ",")
	if len(parts) > 2 {
		return k, false
	}
	for pi, part := range parts {
		digits := strings.TrimRight(part, "nr")
		for _, m := range part[len(digits):] {
			k.modified = true
			if m == 'n' {
				k.numeric = true
			} else {
				k.reverse = true
			}
		}
		n, err := strconv.Atoi(digits)
		if err != nil || n < 1 {
			return k, false
		}
		if pi == 0 {
			k.start = n
		} else {
			k.end = n
		}
	}
	if k.end != 0 && k.end < k.start {
		return k, false
	}
	return k, true
}

func parseSortArgs(args []string) sortArgs {
	var reverse, numeric bool
	var keys []sortKey
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-r":
			reverse = true
		case a == "-n":
			numeric = true
		case a == "-k":
			if i+1 >= len(args) {
				return sortArgs{}
			}
			i++
			k, ok := parseSortKey(args[i])
			if !ok {
				return sortArgs{}
			}
			keys = append(keys, k)
		case strings.HasPrefix(a, "-k"):
			k, ok := parseSortKey(a[2:])
			if !ok {
				return sortArgs{}
			}
			keys = append(keys, k)
		case len(a) > 1 && a[0] == '-' && strings.Trim(a[1:], "rn") == "":
			reverse = reverse || strings.Contains(a, "r")
			numeric = numeric || strings.Contains(a, "n")
		default:
			// -u, -t, an unknown flag, or a file operand.
			return sortArgs{}
		}
	}
	out := sortArgs{plain: true}
	if len(keys) != 1 || keys[0].start != 2 || (keys[0].end != 0 && keys[0].end != 2) {
		return out
	}
	k := keys[0]
	num, rev := numeric, reverse
	if k.modified {
		num, rev = k.numeric, k.reverse
	}
	if num {
		// Ties fall to the whole line, reversed by the global -r alone.
		out.order = &ValueOrder{Reverse: rev, TieReverse: reverse}
	}
	return out
}
