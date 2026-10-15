package ingest

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// validFormats mirrors extractPayload's switch in parse.go — any other
// value silently produces no payload extraction rather than erroring at
// parse time, so this is the one place that actually catches a typo.
var validFormats = map[string]bool{"text": true, "json": true, "prefix_json": true}

// validExpandKeyTypes mirrors ExpandKey's doc comment (the DuckDB column
// types the dashboard/pushdown layer understands).
var validExpandKeyTypes = map[string]bool{"TEXT": true, "INTEGER": true, "DOUBLE": true, "BOOLEAN": true}

// Validate reports every problem with s that would make Parser silently
// extract nothing instead of erroring — parse.go's field extractors
// never return an error for a bad schema (a bad field index, an empty
// format, a startField < 1) they just quietly produce an empty/nil
// result. That's the right behavior for one bad line in a stream, but a
// schema written by hand (as opposed to detected by the setup wizard,
// which only ever emits values these checks accept) needs a place to
// catch the same mistakes before they're silently swallowed in
// production. Returns every problem found, not just the first, so a
// hand-written schema can be fixed in one pass. Empty slice means valid.
func (s *Schema) Validate() []string {
	var errs []string

	if s.Format != "" && !validFormats[s.Format] {
		errs = append(errs, fmt.Sprintf("format: %q is not one of text, json, prefix_json", s.Format))
	}
	if s.Format == "prefix_json" && s.JSONStartField < 1 {
		errs = append(errs, "json_start_field: required (must be >= 1) when format is prefix_json — see extractPayload in ingest/parse.go")
	}

	if s.Envelope != "" && s.Envelope != "none" && legacyEnvelopes[s.Envelope] == nil {
		errs = append(errs, fmt.Sprintf("envelope: %q is not a known legacy envelope; stripEnvelope would silently treat it as none (a prefix is read by field position, not named)", s.Envelope))
	}

	if err := validateFieldRef("timestamp_field", s.TimestampField, s.hasSpec("timestamp")); err != "" {
		errs = append(errs, err)
	} else if !fieldRefDisabled(s.TimestampField) && s.TimestampFormat == "" {
		errs = append(errs, "timestamp_format: required when timestamp_field is set — extractTimestamp returns no timestamp at all without it, silently falling back to ingest time")
	}
	if s.TimestampFields < 0 {
		errs = append(errs, "timestamp_fields: must be >= 0")
	}

	if err := validateFieldRef("level_field", s.LevelField, s.hasSpec("level")); err != "" {
		errs = append(errs, err)
	}
	if err := validateFieldRef("trace_id_field", s.TraceIDField, s.hasSpec("trace_id")); err != "" {
		errs = append(errs, err)
	}
	if err := validateFieldRef("span_name_field", s.SpanNameField, s.hasSpec("span_name")); err != "" {
		errs = append(errs, err)
	}
	if err := validateFieldRef("span_id_field", s.SpanIDField, s.hasSpec("span_id")); err != "" {
		errs = append(errs, err)
	}
	if err := validateFieldRef("parent_span_id_field", s.ParentSpanIDField, s.hasSpec("parent_span_id")); err != "" {
		errs = append(errs, err)
	}
	if err := validateFieldRef("duration_ns_field", s.DurationNsField, s.hasSpec("duration_ns")); err != "" {
		errs = append(errs, err)
	}

	if s.LevelPattern != "" {
		if _, err := regexp.Compile(s.LevelPattern); err != nil {
			errs = append(errs, fmt.Sprintf("level_pattern: %v", err))
		}
	}
	if s.TraceIDPattern != "" {
		if _, err := regexp.Compile(s.TraceIDPattern); err != nil {
			errs = append(errs, fmt.Sprintf("trace_id_pattern: %v", err))
		}
	}
	if s.SpanNamePattern != "" {
		if _, err := regexp.Compile(s.SpanNamePattern); err != nil {
			errs = append(errs, fmt.Sprintf("span_name_pattern: %v", err))
		}
	}
	if s.SpanIDPattern != "" {
		if _, err := regexp.Compile(s.SpanIDPattern); err != nil {
			errs = append(errs, fmt.Sprintf("span_id_pattern: %v", err))
		}
	}
	if s.ParentSpanIDPattern != "" {
		if _, err := regexp.Compile(s.ParentSpanIDPattern); err != nil {
			errs = append(errs, fmt.Sprintf("parent_span_id_pattern: %v", err))
		}
	}
	if s.DurationNsPattern != "" {
		if _, err := regexp.Compile(s.DurationNsPattern); err != nil {
			errs = append(errs, fmt.Sprintf("duration_ns_pattern: %v", err))
		}
	}

	for name, spec := range s.Extract {
		known := false
		for _, n := range valueNames {
			known = known || n == name
		}
		if !known {
			errs = append(errs, fmt.Sprintf("extract: %q is not one of %s", name, strings.Join(valueNames[:], ", ")))
			continue
		}
		for i := 0; i < len(spec.Chars); i++ {
			if spec.Chars[i] >= 0x80 {
				errs = append(errs, fmt.Sprintf("extract.%s.chars: only ASCII characters can be stripped (they are matched bytewise)", name))
				break
			}
		}
		if spec.Fallback != "" {
			re, err := regexp.Compile(spec.Fallback)
			switch {
			case err != nil:
				errs = append(errs, fmt.Sprintf("extract.%s.fallback: %v", name, err))
			case re.NumSubexp() < 1:
				errs = append(errs, fmt.Sprintf("extract.%s.fallback: needs a capture group around the value", name))
			}
		}
	}

	for i, k := range s.ExpandKeys {
		if strings.TrimSpace(k.Key) == "" {
			errs = append(errs, fmt.Sprintf("expand_keys[%d]: key is required", i))
		}
		if !validExpandKeyTypes[k.Type] {
			errs = append(errs, fmt.Sprintf("expand_keys[%d]: type %q is not one of TEXT, INTEGER, DOUBLE, BOOLEAN", i, k.Type))
		}
	}

	return errs
}

// fieldRefDisabled reports whether value means "don't extract" to parse.go's
// field extractors: empty, "none", and — undocumented but real, established
// usage (integration/onboard_test.go's onboarding config) — "0". Every
// extractor guards on `idx < 1`, so 0 has always silently no-op'd exactly
// like empty/none; treating it as a validation error would reject a config
// the parser itself has always accepted harmlessly.
func fieldRefDisabled(value string) bool {
	return value == "" || value == "none" || value == "0"
}

// validateFieldRef checks the shape parse.go's field extractors accept: a
// JSON dot-path (leading '.') or a 1-based positional field index.
// fieldRefDisabled values are always valid (see there). A negative index
// has no established meaning the way 0 does, so it's still flagged — most
// likely a typo or an off-by-one bug, not a deliberate disable. Returns ""
// when valid, a message identifying name otherwise.
func validateFieldRef(name, value string, hasSpec bool) string {
	if fieldRefDisabled(value) {
		return ""
	}
	if strings.HasPrefix(value, ".") {
		if _, err := compileJSONPath(value); err != nil {
			return fmt.Sprintf("%s: %v", name, err)
		}
		return ""
	}
	idx, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Sprintf("%s: %q is neither a JSON path (starting with '.') nor a positional field index", name, value)
	}
	if idx < 0 {
		// Counting from the end exists only for values read by an extract
		// spec; the legacy extractor would silently read nothing.
		if !hasSpec {
			return fmt.Sprintf("%s: field index must be >= 1 (1-based) or 0 to disable, got %d; counting from the end needs an extract entry for this value", name, idx)
		}
		return ""
	}
	return ""
}

// hasSpec reports whether the schema has an extract entry for the named value.
func (s *Schema) hasSpec(name string) bool {
	_, ok := s.Extract[name]
	return ok
}
