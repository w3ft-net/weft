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
	// Visible-line count after wrapping to the current viewport width.
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
func (c *ShellCore) topicDispatch(line string) (string, bool) {
	switch line {
	case "help":
		return helpIndex(), true
	case "help builtins":
		return helpBuiltins(), true
	case "help cron":
		return helpCron(c.tierLimits), true
	case "help alerts":
		return helpAlerts(), true
	case "man alert", "alert --help":
		return AlertManPage, true
	case "man range", "range --help":
		return RangeManPage, true
	case "man less", "less --help":
		return LessManPage, true
	case "man help", "help --help":
		return helpIndex(), true
	}
	return "", false
}

// helpIndex is the top-level `help` page — a short menu of topics.
// The whole point of the restructure is that this stays under a
// dozen lines so it always fits inline without the pager.
func helpIndex() string {
	var b strings.Builder
	b.WriteString(DimText("weft shell"))
	b.WriteString("\n\n")
	b.WriteString(AccentText("Topics"))
	b.WriteString("\n")
	b.WriteString("  help builtins   commands you can use in a pipeline\n")
	b.WriteString("  help cron       scheduled watches, result accessors, tier limits\n")
	b.WriteString("  help alerts     alert conditions, notify.conf, dispatching\n")
	b.WriteString("\n")
	b.WriteString(AccentText("Reference pages"))
	b.WriteString("\n")
	b.WriteString("  man alert       alert grammar and options\n")
	b.WriteString("  man range       set the query time window\n")
	b.WriteString("  man less        the in-shell pager\n")
	b.WriteString("\n")
	b.WriteString(DimText("Tip: long output is paged automatically — press q to return.\n"))
	return b.String()
}

// helpBuiltins lists every command available in a pipeline plus the
// shell's own builtins. Categories are hand-grouped because the
// alphabetical list is useless for discovery.
func helpBuiltins() string {
	var b strings.Builder
	b.WriteString(AccentText("Querying logs"))
	b.WriteString("\n")
	b.WriteString("  cat /log/<app>.log                  stream all lines for an app\n")
	b.WriteString("  head /log/<app>.log                 first N lines (oldest)\n")
	b.WriteString("  tail /log/<app>.log                 last N lines; `tail -f` follows\n")
	b.WriteString("  apps                                list available apps\n")
	b.WriteString("\n")

	b.WriteString(AccentText("Text pipeline"))
	b.WriteString("\n")
	b.WriteString("  grep        filter lines by pattern\n")
	b.WriteString("  awk         field-oriented extraction / predicates\n")
	b.WriteString("  sed         stream substitution\n")
	b.WriteString("  jq          JSON filtering and projection (implies JSON source)\n")
	b.WriteString("  cut, paste  column slicing and joining\n")
	b.WriteString("  sort, uniq  ordering and de-duplication\n")
	b.WriteString("  wc          line/word/byte counts\n")
	b.WriteString("  rev, tac    reverse chars / reverse line order\n")
	b.WriteString("  tee, tr     duplicate to file / translate chars\n")
	b.WriteString("  xargs       build commands from stdin\n")
	b.WriteString("  less        page the final output (see man less)\n")
	b.WriteString("\n")

	b.WriteString(AccentText("Filesystem"))
	b.WriteString("\n")
	b.WriteString("  ls [path]                           list /bin, /cron.d, /etc, /log\n")
	b.WriteString("  cat /bin/<fn>                       view a saved function\n")
	b.WriteString("  cat /cron.d/<watch>                 view a watch definition\n")
	b.WriteString("  rm <path>                           delete a function or watch\n")
	b.WriteString("  vi <path>                           edit (vim keys)\n")
	b.WriteString("  cd <path>                           change the virtual cwd\n")
	b.WriteString("\n")

	b.WriteString(AccentText("Alerting"))
	b.WriteString("\n")
	b.WriteString("  <pipeline> | alert <condition>      see help alerts or man alert\n")
	b.WriteString("\n")

	b.WriteString(AccentText("Shell"))
	b.WriteString("\n")
	b.WriteString("  range                               show or set the query window\n")
	b.WriteString("  which <name>                        show where a command lives\n")
	b.WriteString("  type <name>                         identify a command\n")
	b.WriteString("  history                             past commands\n")
	b.WriteString("  clear                               clear the screen\n")
	b.WriteString("  exit                                quit the shell\n")
	b.WriteString("\n")
	b.WriteString(DimText("Each entry above is a command — `man <name>` where a page exists.\n"))
	return b.String()
}

// helpCron is the watches topic. Reads tier limits from the provider
// so the rendered table matches store/user.go at runtime, not
// whatever was hardcoded at last commit.
func helpCron(limits TierLimitsProvider) string {
	var b strings.Builder
	b.WriteString(AccentText("Watches"))
	b.WriteString("\n")
	b.WriteString("  A watch runs a pipeline on a schedule. Its stdout is recorded\n")
	b.WriteString("  as a cron result; writes to stderr are treated as alert firings\n")
	b.WriteString("  (if the watch has a `| alert` stage — see help alerts).\n\n")

	b.WriteString("  vi /cron.d/<name>       create or edit a watch\n")
	b.WriteString("  cat /cron.d/<name>      view a watch definition\n")
	b.WriteString("  ls /cron.d              list existing watches\n")
	b.WriteString("  rm /cron.d/<name>       delete a watch (stops the schedule)\n\n")

	b.WriteString(AccentText("Watch file format"))
	b.WriteString("\n")
	b.WriteString("  # schedule: every 1m          (or cron: */5 * * * *)\n")
	b.WriteString("  # notify: ops                 (alias from /etc/notify.conf)\n")
	b.WriteString("  cat /log/api.log | grep ERROR | wc -l\n\n")

	b.WriteString(AccentText("Accessing past results"))
	b.WriteString("\n")
	b.WriteString("  weft.latest <watch>                most recent value\n")
	b.WriteString("  weft.avg <watch> <duration>        rolling average over window\n")
	b.WriteString("  weft.max <watch> <duration>        window maximum\n")
	b.WriteString("  weft.min <watch> <duration>        window minimum\n")
	b.WriteString("  weft.sum <watch> <duration>        window sum\n")
	b.WriteString("  weft.stddev <watch> <duration>     window standard deviation\n\n")
	b.WriteString("  Example:\n")
	b.WriteString("    echo $(weft.avg error-rate 1h) | alert above 50 --notify=ops\n\n")

	b.WriteString(AccentText("Tier limits"))
	b.WriteString("\n")
	if limits != nil {
		b.WriteString(renderTierLimits(limits))
	} else {
		b.WriteString("  (not available in this mode — see your plan in the web UI)\n")
	}
	return b.String()
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
	for _, r := range rows {
		b.WriteString("  ")
		b.WriteString(r.name)
		b.WriteString(strings.Repeat(" ", maxName-len(r.name)+2))
		b.WriteString(fmt.Sprintf("%-14s", r.limit))
		b.WriteString("  ")
		b.WriteString(r.interval)
		b.WriteString("\n")
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

// helpAlerts is the alerting topic. Covers conditions, single-row vs
// multi-row mode at a high level, and notify.conf — the piece that
// was entirely missing from the old help text.
func helpAlerts() string {
	var b strings.Builder
	b.WriteString(AccentText("Alerts"))
	b.WriteString("\n")
	b.WriteString("  An alert evaluates a value from stdin against a condition.\n")
	b.WriteString("  When the condition holds, alert writes to stderr — in a\n")
	b.WriteString("  watch, that stderr is what drives incidents and notifications.\n")
	b.WriteString("  Stdin always passes through to stdout unchanged (tee behavior).\n\n")

	b.WriteString(AccentText("Conditions (summary)"))
	b.WriteString("\n")
	b.WriteString("  above <N>      value greater than N\n")
	b.WriteString("  below <N>      value less than N\n")
	b.WriteString("  eq <N>, neq <N>\n")
	b.WriteString("  changed        value differs from previous run\n")
	b.WriteString("  absent         no input received in the window\n")
	b.WriteString("  rising N%      increase over baseline\n")
	b.WriteString("  falling N%     decrease over baseline\n")
	b.WriteString("  (no keyword)   \"any\" — fires when stdin is non-empty\n\n")
	b.WriteString("  See man alert for grammar, --duration, --average, --msg.\n\n")

	b.WriteString(AccentText("Single-row vs multi-row"))
	b.WriteString("\n")
	b.WriteString("  By default alert treats stdin as one scalar per tick.\n")
	b.WriteString("  With -k <N>, each non-blank line is one row and field N\n")
	b.WriteString("  (1-indexed, whitespace-split) is the value. With -t <N>,\n")
	b.WriteString("  another field names the row for per-tag incident dedup.\n\n")
	b.WriteString("  # per-host disk alert\n")
	b.WriteString("  tail /log/shard.log | jq -r '\"\\(.host) \\(.pct)\"' \\\n")
	b.WriteString("    | alert above 80 -k 2 -t 1 --notify=ops\n\n")

	b.WriteString(AccentText("/etc/notify.conf"))
	b.WriteString("\n")
	b.WriteString("  --notify=<alias> looks up a destination from your\n")
	b.WriteString("  notify.conf — a YAML map of alias → target. Edit with:\n\n")
	b.WriteString("    vi /etc/notify.conf\n\n")
	b.WriteString("  Example:\n")
	b.WriteString("    ops:\n")
	b.WriteString("      slack:   \"#alerts\"\n")
	b.WriteString("      pagerduty: primary\n")
	b.WriteString("    critical:\n")
	b.WriteString("      slack:   \"#oncall\"\n")
	b.WriteString("      pagerduty: primary\n\n")
	b.WriteString("  Slack and PagerDuty credentials are configured separately\n")
	b.WriteString("  in the web UI; notify.conf just names destinations.\n\n")

	b.WriteString(AccentText("Testing an alias"))
	b.WriteString("\n")
	b.WriteString("  echo \"test fire\" | alert --notify=ops\n\n")
	b.WriteString("  Interactive runs dispatch immediately; they don't\n")
	b.WriteString("  open an incident (incidents are a cron-watch concept).\n")
	return b.String()
}
