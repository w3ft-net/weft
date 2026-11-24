// Package cronauthor is how a cron is written: the file format it has under
// /cron.d, what its pipeline must satisfy (its output schema, its single alert,
// the numeric reads it makes), and the Service that applies the plan limits and
// the cross-domain checks before it is saved over cron.
//
// Plan limits come from identity.Entitlements and nothing here knows what a plan
// is: a build with none runs unlimited.
package cronauthor

import (
	"fmt"
	"github.com/w3ft-net/weft/fieldspec"
	"strings"

	"github.com/graybeardcorp/warp/pkg/bash"
	"github.com/w3ft-net/weft/alert"
	"github.com/w3ft-net/weft/backends/cron"
	"github.com/w3ft-net/weft/binscript"
)

// Compiled is what the caller parses out of a pipeline at save time: the schema
// the watch row is stamped with, and the crons the pipeline reads as numbers.
type Compiled struct {
	cron.Compiled
	// NumericRefs are the crons this pipeline reads with a numeric weft::
	// function, by literal name; a name built at run time isn't listed.
	NumericRefs []string
}

// valueTypeHeader reads a cron's `# type:` header; text is the only type
// it names, and a cron without one stores numbers.
func valueTypeHeader(pipeline string) (string, error) {
	typ := cron.ValueNumber
	seen := false
	for _, line := range strings.Split(pipeline, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "# type:") {
			continue
		}
		if seen {
			return "", fmt.Errorf("'# type:' is given twice")
		}
		seen = true
		if v := strings.TrimSpace(strings.TrimPrefix(trimmed, "# type:")); v != cron.ValueText {
			return "", fmt.Errorf("'# type:' takes text, got %q; a cron without it stores numbers", v)
		}
		typ = cron.ValueText
	}
	return typ, nil
}

// schemaHeaders reads a cron's `# value:` and `# tag:` header comments.
// They name JSON paths only: text output has a fixed positional rule.
func schemaHeaders(pipeline string) (value, tag fieldspec.FieldSpec, err error) {
	seen := map[string]bool{}
	for _, line := range strings.Split(pipeline, "\n") {
		trimmed := strings.TrimSpace(line)
		var key string
		switch {
		case strings.HasPrefix(trimmed, "# value:"):
			key = "value"
		case strings.HasPrefix(trimmed, "# tag:"):
			key = "tag"
		default:
			continue
		}
		if seen[key] {
			return value, tag, fmt.Errorf("'# %s:' is given twice", key)
		}
		seen[key] = true
		src := strings.TrimSpace(strings.TrimPrefix(trimmed, "# "+key+":"))
		spec, perr := fieldspec.ParseFieldSpec(src)
		if perr != nil || !spec.IsJSON() {
			return value, tag, fmt.Errorf("'# %s:' takes a JSON path like .host, got %q; text output is <value> [<tag>] per line and needs no header", key, src)
		}
		if key == "value" {
			value = spec
		} else {
			tag = spec
		}
	}
	if !tag.IsZero() && value.IsZero() {
		return value, tag, fmt.Errorf("'# tag:' needs a '# value:' header")
	}
	return value, tag, nil
}

// Compile validates pipeline-shape invariants and returns the
// cron's output schema and the compiled bits of its (at most one) `alert`
// stage.
//
// Validation rules:
//   - The pipeline must parse as bash.
//   - `alert` may appear at most once in the pipeline. The single stage is
//     the sole incident-dedup boundary per watch run.
//   - `-v` / `-t` must be a positive integer or a `.`-prefixed JSON path
//     if present (enforced by alert.ParseArgs).
//   - `# value:` / `# tag:` headers name JSON paths, and a cron with an
//     alert stage declares its schema with -v/-t instead.
//
// The AST walk catches alerts nested in command substitutions, compound
// commands, and function bodies so the only way past validation is alias
// or variable obfuscation.
func Compile(pipeline string) (Compiled, error) {
	var out Compiled
	script, err := bash.Parse(pipeline)
	if err != nil {
		return out, fmt.Errorf("parse: %w", err)
	}
	if err := binscript.ValidateAlertPosition(script); err != nil {
		return out, err
	}
	if err := binscript.ValidateWeftFunctionsAreNotPiped(script); err != nil {
		return out, err
	}
	headerValue, headerTag, err := schemaHeaders(pipeline)
	if err != nil {
		return out, err
	}
	if out.ValueType, err = valueTypeHeader(pipeline); err != nil {
		return out, err
	}
	if out.NumericRefs, err = binscript.BashRefs(script); err != nil {
		return out, err
	}
	var alertCmds []*bash.SimpleCommand
	bash.Walk(script, func(n bash.Node) bool {
		if sc, ok := n.(*bash.SimpleCommand); ok {
			if len(sc.Words) > 0 && sc.Words[0].Literal() == "alert" {
				alertCmds = append(alertCmds, sc)
			}
		}
		return true
	})
	if len(alertCmds) > 1 {
		return out, fmt.Errorf("alert may only appear once in a pipeline (found %d)", len(alertCmds))
	}
	if len(alertCmds) == 0 {
		if !headerValue.IsZero() {
			s := headerValue.String()
			out.ValueField = &s
		}
		if !headerTag.IsZero() {
			s := headerTag.String()
			out.TagField = &s
		}
		return out, nil
	}
	out.HasAlert = true
	argv := make([]string, 0, len(alertCmds[0].Words)-1)
	for _, w := range alertCmds[0].Words[1:] {
		argv = append(argv, w.Literal())
	}
	parsed, perr := alert.ParseArgs(argv)
	if perr != nil {
		return out, fmt.Errorf("alert: %w", perr)
	}
	if !headerValue.IsZero() {
		if !parsed.Key.IsZero() || !parsed.Tag.IsZero() {
			return out, fmt.Errorf("this cron sets its schema twice (# value: and alert -v); keep one")
		}
		return out, fmt.Errorf("a cron that alerts declares its schema with alert -v %s, not '# value:'", headerValue)
	}
	if !parsed.Key.IsZero() {
		s := parsed.Key.String()
		out.ValueField = &s
	}
	if !parsed.Tag.IsZero() {
		s := parsed.Tag.String()
		out.TagField = &s
	}
	switch parsed.Condition {
	case alert.Any, alert.Absent, alert.Changed:
	case alert.Matches:
		if out.ValueType != cron.ValueText {
			return out, fmt.Errorf("alert matches reads text; add '# type: text' to store this cron's values as text")
		}
	default:
		if out.ValueType == cron.ValueText {
			return out, fmt.Errorf("this cron stores text (# type: text), and alert %s compares numbers; use any, absent, changed or matches", parsed.Condition)
		}
	}
	// Stamp the condition + numeric threshold for dashboard line
	// rendering. Only above/below/eq/neq carry a single absolute
	// threshold — other conditions leave both fields nil.
	switch parsed.Condition {
	case alert.Above, alert.Below, alert.Eq, alert.Neq:
		cond := parsed.Condition.String()
		thr := parsed.Threshold
		out.AlertCondition = &cond
		out.AlertThreshold = &thr
	}
	return out, nil
}

// Validate is the error-only form of Compile for
// callers that don't need the compiled extractors.
func Validate(pipeline string) error {
	_, err := Compile(pipeline)
	return err
}
