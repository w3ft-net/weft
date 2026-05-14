package shell

import (
	"fmt"
	"strings"
	"time"

	"github.com/kevlarcon/warp/pkg/shellcore"
	"github.com/kevlarcon/weft/internal/timerange"
)

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
