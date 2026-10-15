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
func RunSetupFromLines(app string, lines []string, det *ingest.Detection, probe Prober) (*ingest.Schema, error) {
	if probe == nil {
		probe = LocalProber(lines)
	}
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

	// --- JSON tail confirmation ---
	// Settled before extraction so the probe sees the final format.
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

	// --- Values to extract ---
	// Timestamp, level, trace ID and the span fields each walk the same
	// decision tree (see ExtractField). Lines are the raw sample, not
	// det.Lines, so the line numbers shown match what the browser shows.
	fmt.Println()
	if err := ExtractAll(terminalUI{}, probe, schema, lines, det, app); err != nil {
		return nil, err
	}

	// --- Summary ---
	fmt.Println()
	fmt.Println(titleStyle.Render("Schema summary:"))
	fmt.Printf("  %s %s\n", labelStyle.Render("Service:"), schema.Service)
	fmt.Printf("  %s %s\n", labelStyle.Render("Format:"), schema.Format)
	for _, row := range []struct {
		label string
		f     ingest.ProbeField
	}{
		{"Timestamp:", ingest.ProbeTimestamp}, {"Level:", ingest.ProbeLevel}, {"Trace ID:", ingest.ProbeTraceID},
		{"Span name:", ingest.ProbeSpanName}, {"Span ID:", ingest.ProbeSpanID},
		{"Parent span ID:", ingest.ProbeParentSpanID}, {"Duration:", ingest.ProbeDurationNs},
	} {
		switch ex := extractorFromSchema(schema, row.f); {
		case ex != nil:
			fmt.Printf("  %s %s\n", labelStyle.Render(row.label), describeExtractor(ex))
		case row.f == ingest.ProbeTimestamp:
			fmt.Printf("  %s ingestion time\n", labelStyle.Render(row.label))
		default:
			fmt.Printf("  %s none\n", labelStyle.Render(row.label))
		}
	}
	if schema.Format == "prefix_json" {
		fmt.Printf("  %s field %d+\n", labelStyle.Render("JSON payload:"), schema.JSONStartField)
	}

	return schema, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
