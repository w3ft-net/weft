package setup

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/graybeardcorp/weft/ingest"
)

// fieldNames is how each value is called in the prompts.
var fieldNames = map[ingest.ProbeField]string{
	ingest.ProbeTimestamp:    "timestamp",
	ingest.ProbeLevel:        "log level",
	ingest.ProbeTraceID:      "trace ID",
	ingest.ProbeSpanName:     "span name",
	ingest.ProbeSpanID:       "span ID",
	ingest.ProbeParentSpanID: "parent span ID",
	ingest.ProbeDurationNs:   "span duration",
}

// terminalUI is the interactive UI: huh confirms, plus bubbletea models for
// the cursor picker and the review list.
type terminalUI struct{}

func (terminalUI) Notify(msg string) {
	fmt.Println(flagStyle.Render(msg))
}

func (terminalUI) ConfirmCandidate(f ingest.ProbeField, lines []string, ex *ingest.ProbeExtractor, res *ingest.ProbeResult) (bool, error) {
	name := fieldNames[f]
	fmt.Println()
	fmt.Println(labelStyle.Render(strings.ToUpper(name[:1])+name[1:]) + " " +
		dimStyle.Render(fmt.Sprintf("(%s; found on %d of %d lines)", describeExtractor(ex), res.Matched, res.Total)))
	fmt.Println("  " + dimStyle.Render("sees: "+strings.Join(distinctValues(res, 8), ", ")))
	// A few lines where it was found, with the value highlighted.
	shown := 0
	for i, r := range res.Results {
		if r.Matched && shown < 5 {
			fmt.Println("  " + renderRow(lines[i], r, 110))
			shown++
		}
	}
	var ok bool
	err := huh.NewConfirm().
		Title(fmt.Sprintf("Is that the %s?", name)).
		Affirmative("Yes").Negative("No").
		Value(&ok).Run()
	return ok, err
}

func (terminalUI) WantSpans() (bool, error) {
	var ok bool
	err := huh.NewConfirm().
		Title("Does this service log spans (span name, ID, parent, duration)?").
		Description("Only if it uses weft-go or something like it. Say no if you're unsure.").
		Affirmative("Yes").Negative("No").
		Value(&ok).Run()
	return ok, err
}

func (terminalUI) DurationUnit(sample []string) (string, error) {
	unit := "ns"
	err := huh.NewSelect[string]().
		Title("What are these numbers counted in?").
		Description("You marked "+strings.Join(sample, ", ")+". Durations are stored as nanoseconds.").
		Options(
			huh.NewOption("nanoseconds", "ns"),
			huh.NewOption("microseconds", "us"),
			huh.NewOption("milliseconds", "ms"),
			huh.NewOption("seconds", "s"),
		).
		Value(&unit).Run()
	return unit, err
}

func (terminalUI) ExpectOnLines(f ingest.ProbeField) (bool, error) {
	var ok bool
	err := huh.NewConfirm().
		Title(fmt.Sprintf("Do you expect a %s on lines like these?", fieldNames[f])).
		Description("If this service does log one, we'll help you point at it.").
		Affirmative("Yes").Negative("No, skip it").
		Value(&ok).Run()
	return ok, err
}

func (terminalUI) PickValue(f ingest.ProbeField, lines []string, line int, hint string) (PickResult, error) {
	m := newPicker(fieldNames[f], hint, lines[line], line, len(lines))
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return PickResult{}, err
	}
	return final.(*pickerModel).result, nil
}

func (terminalUI) Review(f ingest.ProbeField, lines []string, ex *ingest.ProbeExtractor, res *ingest.ProbeResult, marks []ingest.ProbeMark) (ReviewResult, error) {
	m := newReview(fieldNames[f], lines, ex, res, marks)
	final, err := tea.NewProgram(m).Run()
	if err != nil {
		return ReviewResult{}, err
	}
	return final.(*reviewModel).result, nil
}
