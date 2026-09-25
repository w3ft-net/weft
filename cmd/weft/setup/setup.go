// Package setup hosts the interactive schema-detection wizard that the
// `weft source add` and `weft source configure` subcommands drive against a
// live sample fetched server-side. Lives under cmd/weft because
// it depends on huh + lipgloss (TUI form library + ANSI styling)
// and is meaningless outside a CLI tty — keeping it out of the
// shell/ package is what lets shell/ build under GOOS=js/GOARCH=wasm
// so the browser shell can reuse the same ConfigureCore wiring as
// the CLI.
package setup

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/graybeardcorp/weft/ingest"
)

var dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Inline(true)

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).MarginBottom(1)
	sampleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).PaddingLeft(2)
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	labelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
)

// RunSetupFromLines runs the interactive schema setup wizard from
// pre-loaded lines (a live sample fetched server-side) and det, the
// server-computed detection for that sample (heuristic envelope/JSON
// detection, plus LLM-classified timestamp/level/trace/span candidates —
// see ingest/llmdetect.go). The caller fetches det from
// SourceSchema.Detection; passing nil here falls back to recomputing the
// (heuristic-only) detection locally, for a row predating classification
// or one whose async classification hasn't landed yet.
//
// The resulting schema is returned to the caller, which posts it to the
// coordinator — it's never written to disk. Customer is not captured on
// the schema either; the api server derives it from the caller's JWT at
// upload time.
func RunSetupFromLines(app string, lines []string, det *ingest.Detection) (*ingest.Schema, error) {
	fmt.Println(titleStyle.Render("schema detection for " + app))
	fmt.Println()

	if det == nil {
		det = ingest.Detect(lines)
	}

	// Show sample lines.
	fmt.Println(labelStyle.Render("Sample lines (after cleanup):"))
	showCount := 5
	if len(det.Lines) < showCount {
		showCount = len(det.Lines)
	}
	for i := 0; i < showCount; i++ {
		line := det.Lines[i]
		if len(line) > 120 {
			line = line[:117] + "..."
		}
		fmt.Println(sampleStyle.Render(line))
	}
	fmt.Println()

	schema := &ingest.Schema{
		Service: app,
	}

	// --- Envelope ---
	if det.HasKevconsEnvelope {
		fmt.Println(successStyle.Render("✓") + " Detected kevcons envelope " +
			dimStyle.Render(fmt.Sprintf("(%d instances: %s)", len(det.Instances), strings.Join(det.Instances, ", "))))
		fmt.Println(labelStyle.Render("  After stripping:"))
		for i := 0; i < showCount && i < len(det.StrippedLines); i++ {
			line := det.StrippedLines[i]
			if len(line) > 120 {
				line = line[:117] + "..."
			}
			fmt.Println(sampleStyle.Render(line))
		}
		fmt.Println()

		var confirm bool
		huh.NewConfirm().
			Title("Strip kevcons [timestamp] [instance] envelope?").
			Value(&confirm).
			Affirmative("Yes").
			Negative("No").
			Run()
		if confirm {
			schema.Envelope = "kevcons"
		}
	}

	// --- Format ---
	suggestedFormat := det.SuggestFormat()
	var format string

	formatOptions := []huh.Option[string]{
		huh.NewOption("Text (whitespace-delimited fields)", "text"),
		huh.NewOption("JSON (entire line is JSON)", "json"),
		huh.NewOption(fmt.Sprintf("Prefix + JSON (JSON starts at field %d)", det.JSONTailStartField), "prefix_json"),
	}

	huh.NewSelect[string]().
		Title("What format are these log lines?").
		Options(formatOptions...).
		Value(&format).
		Run()

	if format == "" {
		format = suggestedFormat
	}
	schema.Format = format

	if format == "prefix_json" {
		schema.JSONStartField = det.JSONTailStartField
	}

	// --- Timestamp ---
	// Selected by index into det.TimestampCandidates, not by encoding the
	// candidate into the option's string value — a Go time layout
	// ("2006-01-02T15:04:05Z07:00") and a JSON path can both contain the
	// ':' a delimited encoding would need, which breaks exactly the way
	// promptFieldCandidate's doc comment calls out for the tag fields.
	tsOptions := []huh.Option[string]{
		huh.NewOption("None (use ingestion time)", "none"),
	}
	for i, tc := range det.TimestampCandidates {
		var label string
		if tc.JSONPath != "" {
			label = fmt.Sprintf("%s: %s", tc.JSONPath, tc.Sample)
		} else {
			label = fmt.Sprintf("Field %d: %s", tc.FieldIndex, tc.Sample)
			if tc.SpanFields > 1 {
				label = fmt.Sprintf("Fields %d-%d: %s", tc.FieldIndex, tc.FieldIndex+tc.SpanFields-1, tc.Sample)
			}
		}
		tsOptions = append(tsOptions, huh.NewOption(label, strconv.Itoa(i)))
	}

	var tsChoice string
	huh.NewSelect[string]().
		Title("Which field is the event timestamp?").
		Options(tsOptions...).
		Value(&tsChoice).
		Run()

	if idx, err := strconv.Atoi(tsChoice); err == nil && idx >= 0 && idx < len(det.TimestampCandidates) {
		tc := det.TimestampCandidates[idx]
		if tc.JSONPath != "" {
			schema.TimestampField = tc.JSONPath
		} else {
			schema.TimestampField = strconv.Itoa(tc.FieldIndex)
			schema.TimestampFields = tc.SpanFields
		}
		schema.TimestampFormat = tc.Format
	}

	// --- Level ---
	levelRef, levelPattern := promptFieldCandidate("Which field contains the log level?", det.LevelCandidates)
	schema.LevelField = levelRef
	schema.LevelPattern = levelPattern

	// --- Trace ID ---
	schema.TraceIDField, schema.TraceIDPattern = promptFieldCandidate("Which field contains the trace/correlation ID? (optional)", det.TraceIDCandidates)

	// --- Span fields ---
	schema.SpanNameField, schema.SpanNamePattern = promptFieldCandidate("Which field contains the span name? (optional)", det.SpanNameCandidates)
	schema.SpanIDField, schema.SpanIDPattern = promptFieldCandidate("Which field contains the span ID? (optional)", det.SpanIDCandidates)
	schema.ParentSpanIDField, schema.ParentSpanIDPattern = promptFieldCandidate("Which field contains the parent span ID? (optional)", det.ParentSpanIDCandidates)
	schema.DurationNsField, schema.DurationNsPattern = promptFieldCandidate("Which field contains the span duration? (optional)", det.DurationNsCandidates)

	// --- JSON tail confirmation ---
	if format != "json" && det.JSONTailCoverage > 0.3 && format != "prefix_json" {
		fmt.Println()
		fmt.Println(labelStyle.Render(fmt.Sprintf(
			"%.0f%% of lines have JSON starting at field %d.",
			det.JSONTailCoverage*100, det.JSONTailStartField)))

		var useJSONTail bool
		huh.NewConfirm().
			Title("Treat the tail as a JSON payload?").
			Value(&useJSONTail).
			Affirmative("Yes").
			Negative("No").
			Run()
		if useJSONTail {
			schema.Format = "prefix_json"
			schema.JSONStartField = det.JSONTailStartField
		}
	}

	// --- Summary ---
	fmt.Println()
	fmt.Println(titleStyle.Render("Schema summary:"))
	fmt.Printf("  %s %s\n", labelStyle.Render("Service:"), schema.Service)
	fmt.Printf("  %s %s\n", labelStyle.Render("Envelope:"), orDefault(schema.Envelope, "none"))
	fmt.Printf("  %s %s\n", labelStyle.Render("Format:"), schema.Format)
	if schema.TimestampField != "" && schema.TimestampField != "none" {
		fmt.Printf("  %s field %s (%s)\n", labelStyle.Render("Timestamp:"), schema.TimestampField, schema.TimestampFormat)
	} else {
		fmt.Printf("  %s ingestion time\n", labelStyle.Render("Timestamp:"))
	}
	printFieldSummary("Level:", schema.LevelField)
	printFieldSummary("Trace ID:", schema.TraceIDField)
	printFieldSummary("Span name:", schema.SpanNameField)
	printFieldSummary("Span ID:", schema.SpanIDField)
	printFieldSummary("Parent span ID:", schema.ParentSpanIDField)
	printFieldSummary("Duration:", schema.DurationNsField)
	if schema.Format == "prefix_json" {
		fmt.Printf("  %s field %d+\n", labelStyle.Render("JSON payload:"), schema.JSONStartField)
	}

	return schema, nil
}

func printFieldSummary(label, field string) {
	if field != "" && field != "none" {
		fmt.Printf("  %s field %s\n", labelStyle.Render(label), field)
	} else {
		fmt.Printf("  %s none\n", labelStyle.Render(label))
	}
}

// promptFieldCandidate shows a select prompt built from a candidate list —
// the shape shared by level, trace ID, and all four span fields (see
// ingest.FieldCandidate) — and returns the chosen field reference (a
// positional index, a JSON path, or "" for none/disabled) and pattern, in
// the same string form Schema's own *Field/*Pattern pairs expect. Six
// call sites (level + trace_id + 4 span fields) is what justifies this as
// a helper rather than writing the prompt out by hand each time the way
// the original Level-only prompt did.
func promptFieldCandidate(title string, candidates []ingest.FieldCandidate) (fieldRef, pattern string) {
	options := []huh.Option[string]{huh.NewOption("None", "none")}
	for i, c := range candidates {
		var label string
		switch {
		case c.JSONPath != "":
			label = fmt.Sprintf("%s: %s", c.JSONPath, c.Sample)
		case c.FieldIndex >= 1:
			label = fmt.Sprintf("Field %d: %s", c.FieldIndex, c.Sample)
		default:
			label = fmt.Sprintf("(whole line): %s", c.Sample)
		}
		if c.Pattern != "" {
			label += dimStyle.Render(fmt.Sprintf(" (pattern: %s)", c.Pattern))
		}
		options = append(options, huh.NewOption(label, strconv.Itoa(i)))
	}

	var choice string
	huh.NewSelect[string]().
		Title(title).
		Options(options...).
		Value(&choice).
		Run()

	if choice == "" || choice == "none" {
		return "", ""
	}
	idx, err := strconv.Atoi(choice)
	if err != nil || idx < 0 || idx >= len(candidates) {
		return "", ""
	}
	c := candidates[idx]
	switch {
	case c.JSONPath != "":
		return c.JSONPath, c.Pattern
	case c.FieldIndex >= 1:
		return strconv.Itoa(c.FieldIndex), c.Pattern
	default:
		// Whole-line search: schema.go's convention is an empty/"none"
		// field ref with a pattern set — NOT "0", which parse.go's
		// extractors treat as a real (always out-of-range) positional
		// index and would silently never match anything.
		return "none", c.Pattern
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
