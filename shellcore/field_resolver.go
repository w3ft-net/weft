package shellcore

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// FieldSpec selects a value out of a single line of pipeline output.
// It is either a 1-indexed whitespace column ("1", "2", …) or a
// dot-prefixed gjson path against the line parsed as JSON (".foo",
// ".foo.bar", ".items.0.name"). Used by both the chart pipe-stage
// and the alert builtin's -k / -t selectors so JSONL pipelines can
// address fields by name without an `awk` step in between.
type FieldSpec struct {
	// Index is 1-indexed and >0 when the spec is positional. Zero
	// means the spec is JSON-path (Path is set) or empty.
	Index int
	// Path is the gjson path without the leading dot, e.g. "foo.bar"
	// for input ".foo.bar". Empty when Index>0 or when the spec is
	// empty. Passed verbatim to gjson.Get, so the full gjson path
	// grammar is supported (numeric segments index arrays).
	Path string
}

// IsZero reports whether the spec was never set.
func (f FieldSpec) IsZero() bool { return f.Index == 0 && f.Path == "" }

// IsJSON reports whether the spec is a JSON path.
func (f FieldSpec) IsJSON() bool { return f.Path != "" }

// String renders the spec back to its source form ("3" or ".foo.bar").
// Used for error messages.
func (f FieldSpec) String() string {
	if f.Path != "" {
		return "." + f.Path
	}
	if f.Index > 0 {
		return strconv.Itoa(f.Index)
	}
	return ""
}

// ParseFieldSpec parses a flag value as either a 1-indexed positional
// column or a dot-prefixed JSON path. Empty strings return a zero spec
// without error so callers can treat "flag absent" the same as "flag
// rejected"; an explicit lone dot ("." with nothing after) is rejected
// because it has no meaningful resolution.
func ParseFieldSpec(s string) (FieldSpec, error) {
	if s == "" {
		return FieldSpec{}, nil
	}
	if strings.HasPrefix(s, ".") {
		path := strings.TrimPrefix(s, ".")
		if path == "" {
			return FieldSpec{}, fmt.Errorf("empty JSON path %q", s)
		}
		return FieldSpec{Path: path}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return FieldSpec{}, fmt.Errorf("expected 1-indexed column or .json.path, got %q", s)
	}
	return FieldSpec{Index: n}, nil
}

// ResolveField extracts the value selected by spec from one line of
// pipeline output.
//
// For positional specs the line is split on whitespace and the
// 1-indexed column is returned (or "" if the line has fewer fields).
// For JSON-path specs the line is parsed as a single JSON value via
// gjson and the path is walked; missing keys return "". Numeric and
// boolean leaves are stringified; objects/arrays are returned as
// their compact JSON encoding.
func ResolveField(spec FieldSpec, line string) (string, error) {
	if spec.IsZero() {
		return "", fmt.Errorf("empty field spec")
	}
	if !spec.IsJSON() {
		fields := strings.Fields(line)
		if spec.Index < 1 || spec.Index > len(fields) {
			return "", nil
		}
		return fields[spec.Index-1], nil
	}
	// gjson.Get is single-pass and allocation-free for primitive
	// leaves — the hot path for chart's per-row extraction.
	if !gjson.Valid(line) {
		return "", fmt.Errorf("invalid json")
	}
	r := gjson.Get(line, spec.Path)
	if !r.Exists() {
		return "", nil
	}
	return r.String(), nil
}
