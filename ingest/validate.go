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

	if s.Envelope != "" && s.Envelope != "none" && s.Envelope != "kevcons" {
		errs = append(errs, fmt.Sprintf("envelope: %q is not one of none, kevcons — stripEnvelope only recognizes \"kevcons\"; anything else is silently treated as no envelope", s.Envelope))
	}

	if err := validateFieldRef("timestamp_field", s.TimestampField); err != "" {
		errs = append(errs, err)
	} else if s.TimestampField != "" && s.TimestampField != "none" && s.TimestampFormat == "" {
		errs = append(errs, "timestamp_format: required when timestamp_field is set — extractTimestamp returns no timestamp at all without it, silently falling back to ingest time")
	}
	if s.TimestampFields < 0 {
		errs = append(errs, "timestamp_fields: must be >= 0")
	}

	if err := validateFieldRef("level_field", s.LevelField); err != "" {
		errs = append(errs, err)
	}
	if err := validateFieldRef("trace_id_field", s.TraceIDField); err != "" {
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

// validateFieldRef checks the shape parse.go's field extractors accept:
// a JSON dot-path (leading '.') or a 1-based positional field index.
// Empty and "none" both mean "don't extract" and are always valid.
// Returns "" when valid, a message identifying name otherwise.
func validateFieldRef(name, value string) string {
	if value == "" || value == "none" || strings.HasPrefix(value, ".") {
		return ""
	}
	idx, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Sprintf("%s: %q is neither a JSON dot-path (starting with '.') nor a positional field index", name, value)
	}
	if idx < 1 {
		return fmt.Sprintf("%s: field index must be >= 1 (1-based), got %d", name, idx)
	}
	return ""
}
