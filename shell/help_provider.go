package shell

import (
	"fmt"
	"sort"
	"strings"
	"time"

	shellcore "github.com/graybeardcorp/warp/pkg/shell"
)

// TierLimitsProvider lets `help cron` render tier-scoped facts (watch caps,
// minimum schedule interval) for a build that has plans. The open build has none:
// its Shell.Tiers is nil and the topic has no such section.
type TierLimitsProvider interface {
	WatchLimit(tier string) int
	MinWatchInterval(tier string) time.Duration
	Tiers() []string
}

// weftHelp adapts the shell-package help corpus to the
// shellcore.HelpProvider interface. Topics are the keys of
// shellDocTopics; Render returns the body, with `help cron`
// interpolated against the tier-limits provider.
type weftHelp struct {
	tiers TierLimitsProvider
}

func (h weftHelp) Topics() []string {
	out := make([]string, 0, len(shellDocTopics))
	for k := range shellDocTopics {
		out = append(out, k)
	}
	return out
}

func (h weftHelp) Render(topic string) string {
	if topic == "help cron" {
		return helpCron(h.tiers)
	}
	return shellDocTopics[topic]
}

// helpCron returns the crons topic with the tier-limits table spliced into the
// {{TIER_LIMITS}} placeholder, or, with no provider, without the section.
func helpCron(limits TierLimitsProvider) string {
	at := strings.Index(HelpCron, "{{TIER_LIMITS}}")
	if at < 0 {
		return HelpCron
	}
	if limits == nil {
		// The section's heading starts after the last blank line before it.
		if cut := strings.LastIndex(HelpCron[:at], "\n\n"); cut >= 0 {
			return HelpCron[:cut] + "\n"
		}
	}
	return strings.Replace(HelpCron, "{{TIER_LIMITS}}", renderTierLimits(limits), 1)
}

// renderTierLimits formats the watch-cap / min-interval table for
// every tier the provider knows about. Tiers with 0 watches are
// rendered as "no crons" rather than "0 crons, 0s".
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
			r.limit = "no crons"
			r.interval = "—"
		default:
			r.limit = fmt.Sprintf("%d cron%s", cap, plural(cap))
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

// Compile-time check that weftHelp satisfies shellcore.HelpProvider.
var _ shellcore.HelpProvider = weftHelp{}
