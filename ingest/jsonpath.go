package ingest

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// jsonPathSegment is one step of a compiled JSON path: either a map key
// (index == -1) or an array index (key == "").
type jsonPathSegment struct {
	key   string
	index int
}

// compileJSONPath parses a schema field's JSON path ("ts", ".ts",
// ".context.trace_id", ".tags[0].value") into segments, evaluated once per
// Parser construction rather than per row (see NewParser). Only plain key
// access and integer array indices are supported — the shape every
// existing schema and the LLM classifier's proposals use. Anything fancier
// (wildcards, filters, slices) is rejected here rather than silently
// matching the wrong thing at ingest time.
func compileJSONPath(path string) ([]jsonPathSegment, error) {
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return nil, fmt.Errorf("empty JSON path")
	}

	var segs []jsonPathSegment
	for _, part := range strings.Split(path, ".") {
		if part == "" {
			return nil, fmt.Errorf("path %q has an empty segment", path)
		}
		key, rest := part, ""
		if i := strings.IndexByte(part, '['); i >= 0 {
			key, rest = part[:i], part[i:]
		}
		if key != "" {
			segs = append(segs, jsonPathSegment{key: key, index: -1})
		}
		for len(rest) > 0 {
			if rest[0] != '[' {
				return nil, fmt.Errorf("path %q: unsupported segment %q — only plain keys and [N] indices are supported", path, part)
			}
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return nil, fmt.Errorf("path %q: unterminated '[' in segment %q", path, part)
			}
			n, err := strconv.Atoi(rest[1:end])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("path %q: %q is not a non-negative array index", path, rest[1:end])
			}
			segs = append(segs, jsonPathSegment{index: n})
			rest = rest[end+1:]
		}
	}
	if len(segs) == 0 {
		return nil, fmt.Errorf("empty JSON path")
	}
	return segs, nil
}

// evalJSONPath walks segs against a value already produced by
// json.Unmarshal (a map[string]any/[]any/scalar tree) and returns the leaf
// value. Never panics on a shape mismatch — a schema field pointing at a
// path that doesn't exist on a given line is exactly as normal as a
// positional field index past the end of a shorter-than-usual line, and
// every other extractor in parse.go treats that as "extract nothing", not
// an error.
func evalJSONPath(v any, segs []jsonPathSegment) (any, bool) {
	cur := v
	for _, seg := range segs {
		if seg.index >= 0 {
			arr, ok := cur.([]any)
			if !ok || seg.index >= len(arr) {
				return nil, false
			}
			cur = arr[seg.index]
			continue
		}
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg.key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// parseJSONBody finds the first '{' in body and unmarshals from there,
// matching every extractor's existing convention (a positional prefix
// before the JSON tail is fine; anything that doesn't parse just means
// "no JSON here"). Callers parse this once per row and share the result
// across every JSON-path field extractor instead of each one re-parsing
// the same bytes.
func parseJSONBody(body string) (any, bool) {
	idx := strings.Index(body, "{")
	if idx < 0 {
		return nil, false
	}
	var v any
	if err := json.Unmarshal([]byte(body[idx:]), &v); err != nil {
		return nil, false
	}
	return v, true
}

// jsonStringAt evaluates a precompiled path against the shared parsed
// body and returns the value as a string. ok is false if the path didn't
// resolve, the body wasn't JSON, or the value at that path isn't a
// string — the same silent-miss behavior every other extractor has.
func jsonStringAt(jsonBody any, hasJSONBody bool, path []jsonPathSegment) (string, bool) {
	if !hasJSONBody || path == nil {
		return "", false
	}
	val, ok := evalJSONPath(jsonBody, path)
	if !ok {
		return "", false
	}
	str, ok := val.(string)
	return str, ok
}
