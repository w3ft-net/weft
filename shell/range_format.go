package shell

import (
	"fmt"
	"strings"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/internal/timerange"
)

// rangeLeadSegment renders the prompt's time-window bracket (e.g.
// "[1d]") from an un-resolved range spec, for warp's opaque
// SetLeadSegment seam. Returns "" when the spec doesn't parse so the
// caller can leave the segment unset rather than show a bogus window.
// The duration is stable across a relative spec's lifetime, so the
// snapshot taken here is fine for a display-only label.
func rangeLeadSegment(spec string) string {
	start, end, err := timerange.ParseRange(spec)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("[%s]", timerange.FormatDuration(end.Sub(start)))
}

// FormatRange renders the current time range for display in the interactive
// shell — ANSI-colored, with a "(now)" suffix when end ≈ now. The plain
// (uncolored) variant lives in internal/timerange.FormatRangePlain.
//
// Lives in shell/ rather than internal/timerange/ so the parser stays
// stdlib-only; only the in-terminal renderer needs shellcore's ANSI helpers.
func FormatRange(start, end time.Time) string {
	now := time.Now()
	var b strings.Builder

	b.WriteString(shellcore.DimText("range: "))
	b.WriteString(timerange.FormatTimeSide(start))
	b.WriteString(shellcore.DimText(" .. "))
	b.WriteString(timerange.FormatTimeSide(end))

	if now.Sub(end).Abs() < 2*time.Second {
		b.WriteString(shellcore.DimText("  (now)"))
	}

	dur := end.Sub(start)
	b.WriteString(shellcore.DimText(fmt.Sprintf("  [%s]", timerange.FormatDuration(dur))))
	b.WriteByte('\n')

	return b.String()
}
