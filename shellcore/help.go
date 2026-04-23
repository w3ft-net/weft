package shellcore

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// TierLimitsProvider lets the shell render tier-scoped facts (watch
// caps, minimum schedule interval) without importing `store`. The
// shell wires concrete implementations at construction time via
// SetTierLimits; if unset, help content falls back to a generic
// "see your plan" line.
type TierLimitsProvider interface {
	WatchLimit(tier string) int
	MinWatchInterval(tier string) time.Duration
	Tiers() []string
}

// SetTierLimits wires a tier-limits source for `help cron`. Safe to
// leave nil in tests or standalone CLI runs — the help topic will
// just skip the table.
func (c *ShellCore) SetTierLimits(p TierLimitsProvider) { c.tierLimits = p }

// showDoc renders help or man output. Short pages land in scrollback
// so they behave like any other builtin; longer pages enter the
// pager so the user can scroll without losing earlier prompt lines.
// Threshold is "does it fit in the viewport with room for the prompt."
func (c *ShellCore) showDoc(text, title string) {
	lineCount := len(wrapLines(text, c.width))
	// Reserve 2 rows: one for the title, one for the next prompt line.
	if c.height > 0 && lineCount > c.height-2 {
		c.enterLess(text, title)
		return
	}
	c.appendScrollback(text)
}

// topicDispatch routes `help`, `help <topic>`, `man <cmd>`, and
// `<cmd> --help` to the right content producer. Returns (text,
// handled). The caller is responsible for choosing how to present
// the text (inline or via the pager).
//
// Most topics resolve through the generated shellDocTopics map. The
// `help cron` topic is special because it needs runtime tier data
// interpolated into the generated body.
func (c *ShellCore) topicDispatch(line string) (string, bool) {
	if line == "help cron" {
		return helpCron(c.tierLimits), true
	}
	if text, ok := shellDocTopics[line]; ok {
		return text, true
	}
	return "", false
}

// helpCron returns the generated watches topic with the tier-limits
// table spliced into the {{TIER_LIMITS}} placeholder. Keeps runtime
// data out of the markdown source.
func helpCron(limits TierLimitsProvider) string {
	var table string
	if limits != nil {
		table = renderTierLimits(limits)
	} else {
		table = "  (not available in this mode — see your plan in the web UI)"
	}
	return strings.Replace(HelpCron, "{{TIER_LIMITS}}", table, 1)
}

// renderTierLimits formats the watch-cap / min-interval table for
// every tier the provider knows about. Tiers with 0 watches are
// rendered as "no watches" rather than "0 watches, 0s".
func renderTierLimits(p TierLimitsProvider) string {
	tiers := append([]string(nil), p.Tiers()...)
	sort.Strings(tiers)

	type row struct {
		name, limit, interval string
	}
	rows := make([]row, 0, len(tiers))
	maxName := 0
	for _, t := range tiers {
		cap := p.WatchLimit(t)
		min := p.MinWatchInterval(t)
		r := row{name: t}
		switch {
		case cap <= 0:
			r.limit = "no watches"
			r.interval = "—"
		default:
			r.limit = fmt.Sprintf("%d watch%s", cap, plural(cap))
			if min > 0 {
				r.interval = fmt.Sprintf("min %s interval", formatShortDur(min))
			} else {
				r.interval = "no minimum interval"
			}
		}
		rows = append(rows, r)
		if len(t) > maxName {
			maxName = len(t)
		}
	}

	var b strings.Builder
	for i, r := range rows {
		b.WriteString("  ")
		b.WriteString(r.name)
		b.WriteString(strings.Repeat(" ", maxName-len(r.name)+2))
		b.WriteString(fmt.Sprintf("%-14s", r.limit))
		b.WriteString("  ")
		b.WriteString(r.interval)
		if i < len(rows)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

func formatShortDur(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return d.String()
}
