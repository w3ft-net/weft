package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/graybeardcorp/weft/shell"
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
	return "[" + strings.Repeat("#", filled) + strings.Repeat("-", width-filled) + "]"
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
func (p styles) coloredBar(collected, target, width int) string {
	if target <= 0 {
		target = 1
	}
	if collected > target {
		collected = target
	}
	filled := collected * width / target
	return p.empty.Render("[") + p.filled.Render(strings.Repeat("#", filled)) +
		p.empty.Render(strings.Repeat("-", width-filled)+"]")
}

// renderPending writes the pending-sources table: one row per source with a
// fill meter and last-line age, and the newest sampled line beneath it.
// Color follows w's terminal capability (none when piped).
func renderPending(w io.Writer, sources []shell.PendingSource, now time.Time, width int) {
	renderPendingStyled(newStyles(w), w, sources, now, width)
}

func renderPendingStyled(st styles, w io.Writer, sources []shell.PendingSource, now time.Time, width int) {
	fmt.Fprintf(w, "%s %s %s %s %s\n",
		cell(st.head, "SERVICE", 20), cell(st.head, "NAMESPACE", 15),
		cell(st.head, "SAMPLE", pendingBarWidth+9), cell(st.head, "STATUS", 8), st.head.Render("LAST LINE"))
	for _, s := range sources {
		target := s.SampleTarget
		status := st.statusStyle(s.Status)
		fmt.Fprintf(w, "%s %s %s %s %s %s\n",
			cell(st.name, s.Service, 20), cell(st.muted, s.Namespace, 15),
			st.coloredBar(s.SampleCollected, target, pendingBarWidth),
			cell(st.count, fmt.Sprintf("%3d/%-3d", s.SampleCollected, target), 7),
			cell(status, s.Status, 8), st.muted.Render(lastLineText(s, now)))
		if line := lastSampleLine(s.Sample, width-6); line != "" {
			fmt.Fprintf(w, "  %s\n", st.preview.Render("› "+line))
		}
	}
}

func runSourcePendingWatch(remote *shell.HTTPCoordinator, namespace string) {
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	for {
		sources, err := remote.ListPendingSources(namespace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "list pending: %v\n", err)
			os.Exit(1)
		}
		width, _, _ := term.GetSize(int(os.Stdout.Fd()))
		if width <= 0 {
			width = 100
		}
		if tty {
			fmt.Print("\033[H\033[J")
		}
		if len(sources) == 0 {
			fmt.Println("No pending sources.")
			return
		}
		renderPending(os.Stdout, sources, time.Now(), width)
		done := true
		for _, s := range sources {
			if s.Status == "sampling" {
				done = false
			}
		}
		if done {
			fmt.Println("\nAll samples are full. Run: weft source configure <namespace>/<service>")
			return
		}
		fmt.Println(dimText("\nrefreshing every 2s — ctrl-c to stop"))
		time.Sleep(2 * time.Second)
	}
}
