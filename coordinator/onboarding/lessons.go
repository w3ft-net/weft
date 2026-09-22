// Package onboarding holds the lesson definitions for the Loom Inc
// onboarding tour. Lessons are evaluated by api/shell.go after every
// successful pipeline execute: when the just-run pipeline matches the
// current lesson's Action verbatim, the step advances and the next
// lesson's prompt copy is emitted as a special SSE event for the
// frontend to render (and as ANSI text for the CLI).
//
// Matching is exact (modulo surrounding whitespace) against Action —
// the same string the web shell injects into the input buffer. So as
// long as the user hits enter on what we suggested, they advance; if
// they edit it, they don't. The tour overlay exposes a re-paste
// affordance for users who wandered off.
package onboarding

import (
	"regexp"
	"strings"
)

// Lesson is one step of the shell-* onboarding stage. The frontend
// (and CLI) renders Prompt before the user attempts the lesson, and
// the web shell pre-injects Action into the input. Running Action
// (verbatim) advances the step.
type Lesson struct {
	Step   string
	Action string
	Prompt string
	// ActionPattern, when set, makes CheckPipeline accept any pipeline
	// matching this compiled regex instead of requiring an exact match
	// against Action. For a lesson whose command embeds a value the
	// user must discover from their own prior output (e.g. a real
	// trace_id) rather than a fixed literal every user shares. Action
	// still holds an illustrative example, shown as the reference/
	// re-paste text — it will not itself match ActionPattern, which is
	// intentional: re-pasting it unedited should not silently "succeed"
	// without the user substituting a real discovered value.
	ActionPattern *regexp.Regexp
}

// traceIDPipelinePattern matches `trace <id>` where <id> is exactly
// the shape weft-go's NewTraceID() produces: 32 lowercase hex chars.
var traceIDPipelinePattern = regexp.MustCompile(`^trace [0-9a-f]{32}$`)

// lessons is the canonical, ordered list. The first 4 entries ship
// with the initial onboarding cut; the rest are placeholders the
// step state machine knows about (so api/onboarding.go's
// nextOnboardingStep walks past them) and can be filled in as
// follow-ups.
var lessons = []*Lesson{
	{
		Step:   "shell-ls",
		Prompt: "Now we are into the meat of weft -- the shell. A lot will be familiar, and a few things are particular to weft. Let's get started. Hit enter to see what is kicking about in the weft shell.",
		Action: "ls /log",
	},
	{
		Step:   "shell-ls-loom",
		Prompt: "`loom` here is our sole defined namespace. If you had additional namespaces, they would be sibling directories. Now `ls` again to see what's in /log/loom.",
		Action: "tree /log/loom",
	},
	{
		Step:   "shell-range",
		Prompt: "Log paths are /log/<namespace>/<service>/<kind>.log. <kind> is the stream: `main` for regular logs, plus `heartbeats`, `metrics`, and `runtime` if the service ships telemetry. If you label an access token with an environment, the env slots in right after the namespace: /log/<namespace>/<env>/<service>/<kind>.log. Loom's tokens are env-bearing, which is why you'll see /log/loom/prod/api/main.log.\n\nNow have a look at your PS1. See that `[1d]`? That's indicating the time range of log lines that will be returned when you hit one of your aggregate log files. Try changing it: you can run `range 1h..` to set it to the past hour.",
		Action: "range 1h..",
	},
	{
		Step:   "shell-tail",
		Action: "tail /log/loom/prod/api/main.log",
		Prompt: "Now tail some logs to see live API request lines flowing in. (Loom Inc might be a fake company but they have real servers.)",
	},
	{
		Step:   "shell-tail-f",
		Action: "tail -f /log/loom/prod/api/main.log",
		Prompt: "`tail` on its own is a snapshot. Add `-f` and it keeps streaming as new lines arrive, same as any Unix tail. Watch a few lines come in, then stop whenever you're ready to move on.",
	},
	{
		Step:   "shell-cd",
		Action: "cd /log/loom/prod/api",
		Prompt: "This is a virtual filesystem, and it has a working directory like any shell. `cd` into a service's directory and you can drop the long prefix — `tail main.log` instead of the full `/log/loom/prod/api/main.log` path.",
	},
	{
		Step:   "shell-grep-awk",
		Action: "tail /log/loom/prod/api/main.log | grep ERROR | awk '{print HOST, $1, $5}'",
		Prompt: "So obviously we have support for awk and grep, but the more material point is `HOST` in the awk program. Weft augments normal field variables in awk and jq with a number of meta-variables to help you isolate exactly where those log lines were produced. More info in the manual.",
	},
	{
		Step:   "shell-trace-find",
		Action: "tail /log/loom/prod/api/main.log | grep checkout | awk '{print TRACE_ID}'",
		Prompt: "Loom's checkout flow hops from the API to an edge payment gateway to a background fulfillment worker — one user-facing request, three separate services. Each hop is tagged with the same trace_id. Copy one of the IDs printed above; you'll use it in the next step.",
	},
	{
		Step:          "shell-trace",
		Action:        "trace <paste the id you copied>",
		ActionPattern: traceIDPipelinePattern,
		Prompt:        "Now run `trace` on the ID you copied — but edit it in first, don't just re-run the line above verbatim. You'll get every line from every service that touched that one request, ordered by time, each tagged with which service produced it.",
	},
	{
		Step:   "shell-alert",
		Action: "tail /log/loom/prod/api/main.log | grep ERROR | wc -l | alert above 0",
		Prompt: "Weft contains a special builtin called alert. For now, this is just going to log to stderr, but you can get all sorts of alerting targets on a cron (more info shortly). The api/prod stream emits errors at ~8% so this fires every time.",
	},
	{
		Step:   "shell-notify",
		Prompt: "/etc/notify.yaml maps alert aliases to where they go (webhooks, email, Slack, PagerDuty). There is no GUI where you can configure this. Like nearly everything in weft, you do it in the shell.",
		Action: "cat /etc/notify.yaml",
	},
	{
		Step:   "shell-cron",
		Action: "ls -l /cron.d/",
		Prompt: "When you want to monitor the output of a one-liner, you just stick a file in /cron.d/. `cat` one of the examples here. Each cron maintains timeseries data that can be aggregated and charted — `ls -l` shows a sparkline of each cron's recent values right in the listing.",
	},
	{
		Step:   "shell-schedule",
		Action: "cat /cron.d/error-rate",
		Prompt: "Crons are just bash scripts with a leading comment that contains a schedule either in OG cron format or 'every n<interval>'.",
	},
	{
		Step:   "shell-bin-bash",
		Action: "ls /bin/",
		Prompt: "If your monitoring patterns need more than a one-liner or you want to parameterize, just add a script to /bin/. These are available to your entire organization. Bash and lua are the supported languages.",
	},
	{
		Step:   "shell-wit",
		Action: "wit log /bin/scan -p",
		Prompt: "The weft shell includes a lightweight version history tool for bin scripts and cron entries called `wit`. The name might not be clever, but you'll remember exactly what it's for! History tracking is automatic and users are prompted for commit messages in vim whenever they create or modify a file — `-p` shows the diff inline for each version.",
	},
	{
		Step:   "shell-bin-lua",
		Action: "vim /bin/severity",
		Prompt: "Without a shebang, weft assumes your extensionless scripts are bash. You don't need the shebang for lua if you add the extension.\n\nNow that we're in vim, make a few edits to the file and try to save. You'll see that you haven't been granted full permissions in the Loom org so your write access is restricted. Then you can `:q!` to quit.",
	},
	{
		// Step ID is historical ("team-yaml"); the lesson now teaches the
		// read-only /etc/passwd roster + group model that replaced it.
		Step:   "shell-team-yaml",
		Action: "cat /etc/passwd",
		Prompt: "The reason that save failed: permissions are group membership, and you're not in the right group. /etc/passwd is the org's read-only roster — one row per member with their groups. Owners show `(all — implicit)`. The groups are `binmgr`/`cronmgr`/`configmgr` (edit /bin, /cron.d, /etc), a namespace name (read its logs), and `<ns>+tokens` (mint producer tokens for it). Owners manage membership with `adduser <email> <group>` and `deluser <email> <group>` — and `adduser <email>` on its own invites someone new. The Members tab in the dashboard manages the same groups through a checkbox grid.",
	},
	{
		Step:   "shell-clipboard",
		Action: "clipboard /bin/scan",
		Prompt: "When a /bin/ script is worth showing to a teammate, `clipboard` builds a deep link that captures the script path, your current range, and any args, and pushes it onto your system clipboard. Anyone signed in to the same org can paste the URL into a browser to land in the shell with the invocation pre-filled. More in `man clipboard`.",
	},
	{
		Step:   "shell-weftfunc",
		Action: "weft::avg error-rate 1h",
		Prompt: "Once you have some scripts running in cron.d, there are some convenience functions for aggregations namespaced under `weft::<fn>` in bash (or `weft.<fn>(...)` in lua). Most take a duration (`weft::latest` does not); there's also a parallel family of predicates like `weft::is_above` and `weft::is_outlier`. These compose naturally into alerts, e.g. `weft::avg error-rate 1m | alert above 100`. Full reference: `man weft`.",
	},
	{
		Step:   "shell-history",
		Action: "history",
		Prompt: "Everything you've typed this session is right here — handy for re-finding a query or copying one into a cron later.",
	},
	{
		Step:   "shell-tmp",
		Action: "echo hello > /tmp/note",
		Prompt: "If you are working through an issue where it's easier to stage output from some one-liners and run `comm` or `join` on those files, dump them to /tmp. Because scratch files tend to abide by gas laws and fill the size of the container, we automatically delete files that haven't been written to for 48 hrs.",
	},
	{
		Step:   "chart",
		Action: "chart error-rate",
		Prompt: "`chart` is another weft-specific builtin. It'll open a modal in the browser. The CLI will render right in the terminal with as much fidelity as it can.",
	},
	{
		Step:   "shell-chart-pipe",
		Action: "tail /log/loom/prod/api/main.log | grep ERROR | chart -x 1 --bucket 5m",
		Prompt: "`chart` doesn't need a saved cron behind it — pipe any pipeline straight into it and chart it directly, bucketed however you want. `-x 1` tells it to bucket by the timestamp in column 1; with no `-y`, it counts rows per bucket.",
	},
	{
		Step:   "shell-dashboard-yaml",
		Action: "cat /etc/dashboards/prod.yaml",
		Prompt: "You can composite charts into dashboards with a bit YAML. Like with /etc/notify.yaml, the schema will be validated on write.",
	},
	{
		Step:   "shell-dashboard",
		Action: "dashboard prod",
		Prompt: "Just like with `chart`, the `dashboard` builtin works in the terminal, but will rely on more modern rendering tools when in the browser.",
	},
	{
		Step:   "shell-usage",
		Action: "cat /etc/usage",
		Prompt: "Last one: `cat /etc/usage` — weft's tally of bytes ingested per stream, and what you'd be billed at the current tier. When you run this, the tour wraps up and we'll drop you back into your own org.",
	},
}

// Steps returns the ordered list of shell-* step keys defined by
// the lesson array. This is the canonical source for which shell
// lessons the state machine knows about — api/onboarding.go folds
// these into its full sequence (between the non-shell prefix and
// the "done" terminator) so renaming or reordering a lesson here
// automatically updates the advance state machine. Adding a lesson
// to lessons[] and forgetting to update the api-side sequence is
// the brittleness this exists to eliminate.
func Steps() []string {
	out := make([]string, 0, len(lessons))
	for _, l := range lessons {
		out = append(out, l.Step)
	}
	return out
}

// Lookup returns the lesson definition for a given onboarding step,
// or nil if the step isn't a shell-* lesson. Callers in the shell
// path use this to decide whether to even attempt predicate
// matching (most commands won't be in onboarding mode at all).
func Lookup(step string) *Lesson {
	for _, l := range lessons {
		if l.Step == step {
			return l
		}
	}
	return nil
}

// CheckPipeline returns true when the given pipeline matches the
// current lesson. Most lessons match the Action string verbatim
// (after trimming surrounding whitespace) — the web shell injects
// Action into the input buffer, so a user who just hits enter
// advances; any edit they make breaks the match. A lesson with
// ActionPattern set instead accepts any pipeline matching that regex,
// for commands that embed a value the user must discover themselves
// (e.g. a real trace_id) rather than share one fixed literal.
func CheckPipeline(currentStep, pipeline string) bool {
	lesson := Lookup(currentStep)
	if lesson == nil {
		return false
	}
	trimmed := strings.TrimSpace(pipeline)
	if lesson.ActionPattern != nil {
		return lesson.ActionPattern.MatchString(trimmed)
	}
	if lesson.Action == "" {
		return false
	}
	return trimmed == strings.TrimSpace(lesson.Action)
}

// PromptFor returns the rendered prompt copy for a step, or "" if
// the step has no lesson definition. Used by the CLI / web shell to
// surface the next instruction after a successful advance.
func PromptFor(step string) string {
	lesson := Lookup(step)
	if lesson == nil {
		return ""
	}
	return lesson.Prompt
}

// ActionFor returns the lesson's suggested command for a step, or ""
// if the step has no lesson definition. The web shell injects this
// into the input buffer so the user can just hit enter to advance.
func ActionFor(step string) string {
	lesson := Lookup(step)
	if lesson == nil {
		return ""
	}
	return lesson.Action
}
