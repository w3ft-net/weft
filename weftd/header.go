package weftd

import (
	"regexp"
	"strconv"
	"strings"
)

// LineHeader is the structured prefix the weft SDKs write on each line of
// the log file they own.
//
//	\x1e v1 \x1f ts \x1f lvl \x1f trace \x1f span_name \x1f span_id \x1f parent_id \x1f dur_ns \x1f fmt \x1f kind \x1e body
//
// The outer \x1e pair brackets the header; \x1f separates values; the first
// value is the version, which fixes the order and meaning of the rest. An
// empty value means "not supplied".
type LineHeader struct {
	Version  int    // 0 for the legacy trace-only envelope
	TsNs     int64  // unix nanoseconds; 0 when absent
	Level    string // as the SDK wrote it
	TraceID  string
	SpanName string
	SpanID   string
	ParentID string
	DurNs    int64
	Fmt      string // "json" or "text"; empty when absent
	Kind     string // validated; empty means the source's own kind
}

const (
	headerDelim = '\x1e'
	valueDelim  = "\x1f"

	// v1Values is how many values after the version v1 defines. Versions are
	// append-only, so a higher version's first v1Values values are v1's.
	v1Values = 9
)

var (
	versionToken = regexp.MustCompile(`^v([0-9]+)$`)
	kindName     = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

	// Kinds weftd itself writes; a header may not claim them.
	reservedKinds = map[string]bool{
		"heartbeats": true, "runtime": true, "host": true, "disk": true, "network": true,
	}
)

// ParseHeader strips a leading header from line. ok is false when the line
// has none, in which case body is line unchanged.
//
// A header with no \x1f that is not a version token is the legacy socket
// envelope, \x1eTRACE\x1e, and yields a trace ID only. A version higher than
// this weftd knows is read as v1 and its extra values ignored.
func ParseHeader(line string) (h LineHeader, body string, ok bool) {
	if len(line) == 0 || line[0] != headerDelim {
		return h, line, false
	}
	end := strings.IndexByte(line[1:], headerDelim)
	if end < 0 {
		return h, line, false
	}
	raw, body := line[1:1+end], line[1+end+1:]

	if !strings.Contains(raw, valueDelim) && !versionToken.MatchString(raw) {
		h.TraceID = raw
		return h, body, true
	}

	vals := strings.Split(raw, valueDelim)
	m := versionToken.FindStringSubmatch(vals[0])
	if m == nil {
		return h, line, false
	}
	v, _ := strconv.Atoi(m[1])
	if v < 1 {
		return h, line, false
	}
	h.Version = v
	vals = vals[1:]
	if len(vals) > v1Values {
		vals = vals[:v1Values]
	}
	get := func(i int) string {
		if i < len(vals) {
			return vals[i]
		}
		return ""
	}
	h.TsNs = positiveInt(get(0))
	h.Level = get(1)
	h.TraceID = get(2)
	h.SpanName = get(3)
	h.SpanID = get(4)
	h.ParentID = get(5)
	h.DurNs = positiveInt(get(6))
	if f := get(7); f == "json" || f == "text" {
		h.Fmt = f
	}
	if k := get(8); kindName.MatchString(k) && !reservedKinds[k] {
		h.Kind = k
	}
	return h, body, true
}

// positiveInt parses s as a decimal int64, returning 0 for anything that is
// not a positive integer: a bad value is treated as not supplied rather than
// failing the line.
func positiveInt(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// StripSample removes line headers from sample lines, so the wizard and the
// detectors see the same bodies the rows carry. sdkFed reports whether any
// line had a versioned header; format is "json" only when every headered
// line declared json, and "text" otherwise. Lines without a header pass
// through unchanged.
func StripSample(lines []string) (bodies []string, sdkFed bool, format string) {
	bodies = make([]string, len(lines))
	allJSON := true
	for i, l := range lines {
		h, body, ok := ParseHeader(l)
		bodies[i] = body
		if !ok || h.Version < 1 {
			bodies[i] = l
			continue
		}
		sdkFed = true
		if h.Fmt != "json" {
			allJSON = false
		}
	}
	if !sdkFed {
		return bodies, false, ""
	}
	if allJSON {
		return bodies, true, "json"
	}
	return bodies, true, "text"
}
