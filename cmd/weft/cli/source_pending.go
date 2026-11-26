package cli

import (
	"encoding/json"
	"fmt"
	"github.com/w3ft-net/weft/cmd/weft/ui"
	"io"
	"os"
	"strings"
	"time"

	"github.com/w3ft-net/weft/shell"
	"golang.org/x/term"
)

const pendingBarWidth = 20

// progressBar renders "[#####---------]" for collected/target.
func progressBar(collected, target, width int) string {
	if target <= 0 {
		target = 1
	}
	if collected > target {
		collected = target
	}
	filled := collected * width / target
	return "[" + strings.Repeat("■", filled) + strings.Repeat("-", width-filled) + "]"
}

// sinceText renders an RFC3339 timestamp as a compact age ("4s ago"), or "-"
// when the source has not received a line yet.
func sinceText(rfc3339 string, now time.Time) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return "-"
	}
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < 0:
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// lastLineText is the LAST LINE column: how long ago the newest sampled line
// arrived, or that none has since a resample reset the sample.
func lastLineText(s shell.PendingSource, now time.Time) string {
	if s.SampleCollected == 0 && s.SampleUpdatedAt != "" {
		return "waiting for new lines"
	}
	return sinceText(s.SampleUpdatedAt, now)
}

// lastSampleLine is the newest line of a source's sample, truncated to fit
// width, or "" if the sample is empty.
func lastSampleLine(sample json.RawMessage, width int) string {
	var lines []string
	if json.Unmarshal(sample, &lines) != nil || len(lines) == 0 {
		return ""
	}
	line := strings.Join(strings.Fields(lines[len(lines)-1]), " ")
	if r := []rune(line); width > 0 && len(r) > width {
		line = string(r[:width-1]) + "…"
	}
	return line
}

// coloredBar is progressBar with the filled run in gold and the rest muted.
func coloredBar(p ui.Styles, collected, target, width int) string {
	if target <= 0 {
		target = 1
	}
	if collected > target {
		collected = target
	}
	filled := collected * width / target
	return p.Empty.Render("[") + p.Filled.Render(strings.Repeat("■", filled)) +
		p.Empty.Render(strings.Repeat("-", width-filled)+"]")
}

// renderPending writes the pending-sources table: one row per source with a
// fill meter and last-line age, and the newest sampled line beneath it.
// Color follows w's terminal capability (none when piped).
func renderPending(w io.Writer, sources []shell.PendingSource, now time.Time, width int) {
	renderPendingStyled(ui.NewStyles(w), w, sources, now, width)
}

func renderPendingStyled(st ui.Styles, w io.Writer, sources []shell.PendingSource, now time.Time, width int) {
	fmt.Fprintf(w, "%s %s %s %s %s\n",
		ui.Cell(st.Head, "SERVICE", 20), ui.Cell(st.Head, "NAMESPACE", 15),
		ui.Cell(st.Head, "SAMPLE", pendingBarWidth+9), ui.Cell(st.Head, "STATUS", 8), st.Head.Render("LAST LINE"))
	for _, s := range sources {
		target := s.SampleTarget
		status := st.StatusStyle(s.Status)
		fmt.Fprintf(w, "%s %s %s %s %s %s\n",
			ui.Cell(st.Name, s.Service, 20), ui.Cell(st.Muted, s.Namespace, 15),
			coloredBar(st, s.SampleCollected, target, pendingBarWidth),
			ui.Cell(st.Count, fmt.Sprintf("%3d/%-3d", s.SampleCollected, target), 7),
			ui.Cell(status, s.Status, 8), st.Muted.Render(lastLineText(s, now)))
		if line := lastSampleLine(s.Sample, width-6); line != "" {
			fmt.Fprintf(w, "  %s\n", st.Preview.Render("› "+line))
		}
	}
}

func now() time.Time { return time.Now() }

// watchPending redraws the pending table every two seconds until every sample
// is full.
func watchPending(remote *shell.HTTPCoordinator, namespace string) error {
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	for {
		sources, err := remote.ListPendingSources(namespace)
		if err != nil {
			return fmt.Errorf("list pending: %v", err)
		}
		if tty {
			fmt.Print("\033[H\033[J")
		}
		if len(sources) == 0 {
			fmt.Println("No pending sources.")
			return nil
		}
		renderPending(os.Stdout, sources, time.Now(), terminalWidth())
		done := true
		for _, s := range sources {
			if s.Status == "sampling" {
				done = false
			}
		}
		if done {
			fmt.Println("\nAll samples are full. Run: weft source configure <namespace>/<service>")
			return nil
		}
		fmt.Println(dimText("\nrefreshing every 2s — ctrl-c to stop"))
		time.Sleep(2 * time.Second)
	}
}

func dimText(s string) string {
	return "\033[90m" + s + "\033[0m"
}
