package shell

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/kevlarcon/weft/ingest"
)

var dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).Inline(true)

var (
	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6")).MarginBottom(1)
	sampleStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("8")).PaddingLeft(2)
	successStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Bold(true)
	labelStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
)

// RunSetup runs the interactive schema setup wizard from a sample file.
func RunSetup(app, customer, sampleFile string) (*ingest.Schema, error) {
	data, err := os.ReadFile(sampleFile)
	if err != nil {
		return nil, fmt.Errorf("read sample file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	return RunSetupFromLines(app, customer, lines)
}

// RunSetupFromLines runs the interactive schema setup wizard from pre-loaded lines.
func RunSetupFromLines(app, customer string, lines []string) (*ingest.Schema, error) {
	fmt.Println(titleStyle.Render("weft setup — schema detection for " + app))
	fmt.Println()

	// Run detection.
	det := ingest.Detect(lines)

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
		App:      app,
		Customer: customer,
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

	// Work with stripped lines for the rest.
	sampleLines := det.StrippedLines
	if len(sampleLines) == 0 {
		sampleLines = det.Lines
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
	tsOptions := []huh.Option[string]{
		huh.NewOption("None (use ingestion time)", "none"),
	}
	for _, tc := range det.TimestampCandidates {
		label := fmt.Sprintf("Field %d: %s", tc.FieldIndex, tc.Sample)
		if tc.SpanFields > 1 {
			label = fmt.Sprintf("Fields %d-%d: %s", tc.FieldIndex, tc.FieldIndex+tc.SpanFields-1, tc.Sample)
		}
		val := fmt.Sprintf("%d:%d:%s", tc.FieldIndex, tc.SpanFields, tc.Format)
		tsOptions = append(tsOptions, huh.NewOption(label, val))
	}

	var tsChoice string
	huh.NewSelect[string]().
		Title("Which field is the event timestamp?").
		Options(tsOptions...).
		Value(&tsChoice).
		Run()

	if tsChoice != "" && tsChoice != "none" {
		parts := strings.SplitN(tsChoice, ":", 3)
		if len(parts) == 3 {
			schema.TimestampField = parts[0]
			fmt.Sscanf(parts[1], "%d", &schema.TimestampFields)
			schema.TimestampFormat = parts[2]
		}
	}

	// --- Level ---
	levelOptions := []huh.Option[string]{
		huh.NewOption("None (no level field)", "none"),
	}
	for _, lc := range det.LevelCandidates {
		label := fmt.Sprintf("Field %d: %s", lc.FieldIndex, lc.Sample)
		val := fmt.Sprintf("%d", lc.FieldIndex)
		if lc.Pattern != "" {
			val += ":" + lc.Pattern
			label += dimStyle.Render(fmt.Sprintf(" (pattern: %s)", lc.Pattern))
		}
		levelOptions = append(levelOptions, huh.NewOption(label, val))
	}

	var levelChoice string
	huh.NewSelect[string]().
		Title("Which field contains the log level?").
		Options(levelOptions...).
		Value(&levelChoice).
		Run()

	if levelChoice != "" && levelChoice != "none" {
		parts := strings.SplitN(levelChoice, ":", 2)
		schema.LevelField = parts[0]
		if len(parts) > 1 {
			schema.LevelPattern = parts[1]
		}
	}

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
	fmt.Printf("  %s %s\n", labelStyle.Render("App:"), schema.App)
	fmt.Printf("  %s %s\n", labelStyle.Render("Envelope:"), orDefault(schema.Envelope, "none"))
	fmt.Printf("  %s %s\n", labelStyle.Render("Format:"), schema.Format)
	if schema.TimestampField != "" && schema.TimestampField != "none" {
		fmt.Printf("  %s field %s (%s)\n", labelStyle.Render("Timestamp:"), schema.TimestampField, schema.TimestampFormat)
	} else {
		fmt.Printf("  %s ingestion time\n", labelStyle.Render("Timestamp:"))
	}
	if schema.LevelField != "" && schema.LevelField != "none" {
		fmt.Printf("  %s field %s\n", labelStyle.Render("Level:"), schema.LevelField)
	} else {
		fmt.Printf("  %s none\n", labelStyle.Render("Level:"))
	}
	if schema.Format == "prefix_json" {
		fmt.Printf("  %s field %d+\n", labelStyle.Render("JSON payload:"), schema.JSONStartField)
	}

	// --- Save ---
	outPath := fmt.Sprintf("%s.schema.yaml", app)
	var save bool
	huh.NewConfirm().
		Title(fmt.Sprintf("Save schema to %s?", outPath)).
		Value(&save).
		Affirmative("Yes").
		Negative("No").
		Run()

	if save {
		if err := schema.Save(outPath); err != nil {
			return nil, fmt.Errorf("save schema: %w", err)
		}
		fmt.Println(successStyle.Render(fmt.Sprintf("✓ Saved to %s", outPath)))
	}

	return schema, nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
